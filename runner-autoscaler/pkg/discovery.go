package pkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Every page is its own retryable task, so a large organization does not exceed
// one Cloud Run deadline.
type discoveryPage struct {
	Source     string `json:"source,omitempty"`
	Repository string `json:"repository,omitempty"`
	RunID      int64  `json:"run_id,omitempty"`
	Page       int    `json:"page,omitempty"`
}

// discoveryRunWindow bounds the run listing per repository. GitHub cancels a
// job queued for more than a day, so a run created earlier than this cannot
// hold demand the autoscaler still needs to serve.
const discoveryRunWindow = 3 * 24 * time.Hour

// Active run statuses are filtered client-side: one unfiltered listing per
// repository costs a single request, where a per-status listing costs four.
var activeRunStatuses = map[string]bool{"queued": true, "in_progress": true, "waiting": true, "pending": true}

func (s *Autoscaler) discover(c *gin.Context) {
	if !s.privateRequest(c) {
		return
	}
	data, err := s.readBody(c)
	if err != nil {
		return
	}
	var p discoveryPage
	if len(data) > 0 && json.Unmarshal(data, &p) != nil {
		c.AbortWithStatus(400)
		return
	}
	ctx, cancel := s.opContext()
	defer cancel()
	until, err := s.store.Backoff(ctx, "github", time.Time{})
	if err != nil {
		c.AbortWithError(503, err)
		return
	}
	if time.Now().Before(until) {
		c.Status(200)
		return
	}
	if err = s.discoverPage(ctx, p); err != nil {
		log.Errorf("Lifecycle discovery failed: %v", err)
		var delayed retryAtError
		if errors.As(err, &delayed) {
			c.Status(200)
			return
		} // Next scheduled traversal resumes after durable backoff.
		c.AbortWithError(503, err)
		return
	}
	c.Status(200)
}
func (s *Autoscaler) discoverPage(ctx context.Context, p discoveryPage) error {
	if p.Source == "" {
		for _, src := range s.conf.RegisteredSources {
			if src.SourceType == TypeRepository {
				if err := s.queue(ctx, "/discover", "", discoveryPage{Source: src.Name, Repository: src.Name}, 0); err != nil {
					return err
				}
			} else if len(s.conf.DiscoveryRepositories) > 0 {
				for _, repo := range s.conf.DiscoveryRepositories {
					if err := s.queue(ctx, "/discover", "", discoveryPage{Source: src.Name, Repository: repo}, 0); err != nil {
						return err
					}
				}
			} else if src.SourceType == TypeOrganization {
				if err := s.queue(ctx, "/discover", "", discoveryPage{Source: src.Name}, 0); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("enterprise discovery requires discovery_repositories")
			}
		}
		return nil
	}
	src, ok := s.conf.RegisteredSources[p.Source]
	if !ok {
		return fmt.Errorf("unknown discovery source")
	}
	pat, err := s.readPat(ctx)
	if err != nil {
		return err
	}
	return s.discoverWithPAT(ctx, src, pat, p)
}
func (s *Autoscaler) discoverWithPAT(ctx context.Context, src Source, pat string, p discoveryPage) error {
	var err error
	if p.Page < 1 {
		p.Page = 1
	}
	if p.Repository == "" {
		var repos []struct {
			FullName string `json:"full_name"`
			Archived bool   `json:"archived"`
		}
		if err = s.githubGet(ctx, pat, fmt.Sprintf("https://api.github.com/orgs/%s/repos?per_page=100&page=%d", url.PathEscape(src.Name), p.Page), &repos); err != nil {
			return err
		}
		for _, repo := range repos {
			if repo.Archived {
				continue
			}
			if err = s.queue(ctx, "/discover", "", discoveryPage{Source: src.Name, Repository: repo.FullName}, 0); err != nil {
				return err
			}
		}
		if len(repos) == 100 {
			p.Page++
			return s.queue(ctx, "/discover", "", p, 0)
		}
		return nil
	}
	if p.RunID == 0 {
		var runs struct {
			Runs []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			} `json:"workflow_runs"`
		}
		since := time.Now().Add(-discoveryRunWindow).UTC().Format("2006-01-02")
		endpoint := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs?created=%s&per_page=100&page=%d", p.Repository, url.QueryEscape(">="+since), p.Page)
		if err = s.githubGet(ctx, pat, endpoint, &runs); err != nil {
			return err
		}
		for _, run := range runs.Runs {
			if !activeRunStatuses[run.Status] {
				continue
			}
			if err = s.queue(ctx, "/discover", "", discoveryPage{Source: src.Name, Repository: p.Repository, RunID: run.ID}, 0); err != nil {
				return err
			}
		}
		if len(runs.Runs) == 100 {
			p.Page++
			return s.queue(ctx, "/discover", "", p, 0)
		}
		return nil
	}
	var jobs struct {
		Jobs []Job `json:"jobs"`
	}
	if err = s.githubGet(ctx, pat, fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100&page=%d", p.Repository, p.RunID, p.Page), &jobs); err != nil {
		return err
	}
	if err = s.observeDiscoveredJobs(ctx, src, p.Repository, jobs.Jobs); err != nil {
		return err
	}
	if len(jobs.Jobs) == 100 {
		p.Page++
		return s.queue(ctx, "/discover", "", p, 0)
	}
	return nil
}

func (s *Autoscaler) observeDiscoveredJobs(ctx context.Context, src Source, repository string, jobs []Job) error {
	for _, job := range jobs {
		if job.Status != "queued" && job.Status != "in_progress" {
			continue
		}
		if ok, _ := job.HasAnyLabelGroup(s.conf.RunnerLabelGroups); !ok {
			continue
		}
		// Same intake rule as durableWebhook: the deprecated @machine: syntax can
		// never match a registered runner label, so provisioning for it is waste.
		if job.HasLegacyMagicLabel() {
			continue
		}
		job.RepositoryFullName = repository
		if err := s.observe(ctx, src, job, false); err != nil {
			return err
		}
		if err := s.enqueueJob(ctx, src, job, 0); err != nil {
			return err
		}
	}
	return nil
}

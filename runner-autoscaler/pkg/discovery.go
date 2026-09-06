package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Every page is its own retryable task, so a large organization does not exceed
// one Cloud Run deadline. Both queued and in_progress runs can hold queued jobs.
type discoveryPage struct {
	Source     string `json:"source,omitempty"`
	Repository string `json:"repository,omitempty"`
	RunID      int64  `json:"run_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Page       int    `json:"page,omitempty"`
}

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
	if err = s.discoverPage(ctx, p); err != nil {
		log.Errorf("Lifecycle discovery failed: %v", err)
		c.AbortWithError(503, err)
		return
	}
	c.Status(200)
}
func (s *Autoscaler) discoverPage(ctx context.Context, p discoveryPage) error {
	if p.Page < 1 {
		p.Page = 1
	}
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
	if p.RunID == 0 && p.Status == "" {
		for _, status := range []string{"queued", "in_progress", "waiting", "pending"} {
			next := p
			next.Status = status
			if err = s.queue(ctx, "/discover", "", next, 0); err != nil {
				return err
			}
		}
		return nil
	}
	if p.RunID == 0 {
		var runs struct {
			Runs []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		endpoint := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs?status=%s&per_page=100&page=%d", p.Repository, p.Status, p.Page)
		if err = s.githubGet(ctx, pat, endpoint, &runs); err != nil {
			return err
		}
		for _, run := range runs.Runs {
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
	for _, job := range jobs.Jobs {
		if job.Status != "queued" {
			continue
		}
		if ok, _ := job.HasAnyLabelGroup(s.conf.RunnerLabelGroups); !ok {
			continue
		}
		job.RepositoryFullName = p.Repository
		if err = s.observe(ctx, src, job, false); err != nil {
			return err
		}
		if err = s.queue(ctx, s.conf.RouteCreateVm, src.Name, job, 0); err != nil {
			return err
		}
	}
	if len(jobs.Jobs) == 100 {
		p.Page++
		return s.queue(ctx, "/discover", "", p, 0)
	}
	return nil
}

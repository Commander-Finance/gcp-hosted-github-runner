package pkg

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// githubEndpointKey scopes a permission backoff to one resource path. The query
// string is dropped so every page of a denied listing shares the backoff, while
// sibling endpoints under the same repository (job status versus runner
// registration) stay independent.
func githubEndpointKey(endpoint string) string {
	u, _ := url.Parse(endpoint)
	path := strings.Trim(u.Path, "/")
	return fmt.Sprintf("permission-%x", sha256.Sum256([]byte(path)))
}
func githubRetryAt(header http.Header, now time.Time) time.Time {
	until := now.Add(time.Minute)
	if seconds, err := strconv.ParseInt(header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if at, err := http.ParseTime(header.Get("Retry-After")); err == nil && at.After(until) {
		until = at
	}
	if epoch, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil && header.Get("X-RateLimit-Remaining") == "0" {
		at := time.Unix(epoch, 0).Add(time.Second)
		if at.After(until) {
			until = at
		}
	}
	return until
}
func (s *Autoscaler) githubFailure(ctx context.Context, endpoint string, resp *http.Response) error {
	rate := resp.StatusCode == 429 || (resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"))
	if resp.StatusCode == 403 && !rate {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := strings.ToLower(string(body))
		rate = strings.Contains(message, "secondary rate limit") || strings.Contains(message, "abuse detection")
	}
	if rate || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		until := time.Now().Add(15 * time.Minute)
		key := githubEndpointKey(endpoint)
		message := fmt.Sprintf("GitHub permission/not-found response %d; retaining demand and backing off", resp.StatusCode)
		if rate {
			until = githubRetryAt(resp.Header, time.Now())
			key = "github"
			message = "GitHub rate limit; deferring requests until reset"
		}
		if s.store != nil {
			var err error
			until, err = s.store.Backoff(ctx, key, until)
			if err != nil {
				return err
			}
		}
		return retryAtError{until, message}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return permanentError{fmt.Sprintf("GitHub request rejected: %d", resp.StatusCode)}
	}
	return fmt.Errorf("transient GitHub response: %d", resp.StatusCode)
}

package executors

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// waitHealthy polls until a 2xx or the timeout; auth lets the check pass Basic Auth.
func waitHealthy(ctx context.Context, baseURL string, hc *agentv1.HealthcheckConfig, auth *agentv1.BasicAuth, logLine LogFunc) error {
	path := hc.GetPath()
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	timeout := time.Duration(hc.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	url := strings.TrimSuffix(baseURL, "/") + path

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	backoff := 250 * time.Millisecond

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if auth != nil {
			req.SetBasicAuth(auth.GetUser(), auth.GetPassword())
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				logLine(agentv1.LogStream_LOG_STREAM_SYSTEM_OK, fmt.Sprintf("health check passed (%d %s)", resp.StatusCode, path))
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			return &CodedError{
				Code: CodeHealthcheckFailed,
				Err:  fmt.Errorf("health check %s did not pass within %s: %v", path, timeout, lastErr),
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = growBackoff(backoff, 2*time.Second)
	}
}

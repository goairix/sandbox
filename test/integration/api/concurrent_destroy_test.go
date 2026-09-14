package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/pkg/types"
	"github.com/stretchr/testify/require"
)

// TestDeployedOrdinaryConcurrentDestroy only operates on its own newly created
// sandboxes. Supply three different direct per-replica endpoints explicitly.
func TestDeployedOrdinaryConcurrentDestroy(t *testing.T) {
	raw := os.Getenv("SANDBOX_API_TEST_URLS")
	if raw == "" {
		t.Skip("opt-in deployed suite: SANDBOX_API_TEST_URLS")
	}
	urls := strings.Split(raw, ",")
	require.GreaterOrEqual(t, len(urls), 3)
	seen := make(map[string]bool)
	for i := range urls {
		urls[i] = strings.TrimRight(strings.TrimSpace(urls[i]), "/")
		parsed, err := url.Parse(urls[i])
		require.NoError(t, err)
		require.Contains(t, []string{"http", "https"}, parsed.Scheme)
		require.NotEmpty(t, parsed.Host)
		require.Empty(t, parsed.User, "credentials belong in SANDBOX_API_TEST_KEY")
		require.False(t, seen[urls[i]], "supply different direct per-replica URLs")
		seen[urls[i]] = true
	}
	for _, mode := range []string{"ephemeral", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			s := &apiSuite{t: t, ctx: ctx, client: &http.Client{Timeout: 90 * time.Second}, urls: urls, key: os.Getenv("SANDBOX_API_TEST_KEY")}
			sb := s.create(types.CreateSandboxRequest{Mode: mode, Timeout: 600})
			type result struct {
				replica int
				status  int
				body    []byte
				err     error
			}
			start := make(chan struct{})
			results := make(chan result, 3)
			for replica := range 3 {
				go func() {
					<-start
					req, err := http.NewRequestWithContext(ctx, http.MethodDelete, urls[replica]+"/api/v1/sandboxes/"+sb.ID, nil)
					if err != nil {
						results <- result{replica: replica, err: err}
						return
					}
					if s.key != "" {
						req.Header.Set("Authorization", "Bearer "+s.key)
					}
					response, err := s.client.Do(req)
					if err != nil {
						results <- result{replica: replica, err: err}
						return
					}
					body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
					closeErr := response.Body.Close()
					results <- result{replica: replica, status: response.StatusCode, body: body, err: errors.Join(err, closeErr)}
				}()
			}
			close(start)
			// Join every worker before asserting, including when another failed.
			responses := make([]result, 0, 3)
			for range 3 {
				responses = append(responses, <-results)
			}
			for _, response := range responses {
				require.NoError(t, response.err, "replica %d", response.replica)
				require.LessOrEqual(t, len(response.body), 64<<10)
				require.Equal(t, http.StatusOK, response.status, "replica %d: %s", response.replica, response.body)
				var ack map[string]string
				require.NoError(t, json.Unmarshal(response.body, &ack), "an empty implicit 200 is not a cleanup ACK")
				require.Equal(t, "sandbox destroyed", ack["message"])
			}
			for replica := range 3 {
				s.request(replica, http.MethodGet, "/sandboxes/"+sb.ID, nil, http.StatusNotFound)
			}
		})
	}
}

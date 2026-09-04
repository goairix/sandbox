package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewDoesNotDeleteManagedOrphanResources(t *testing.T) {
	var deletePaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/networks"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "isolated-id", "Name": isolatedNetworkName},
				{"Id": "open-id", "Name": openNetworkName},
				{"Id": "pair-id", "Name": pairNetworkPrefix + "orphan"},
			})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "gateway-id"}})
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/networks/pair-id"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         "pair-id",
				"Name":       pairNetworkPrefix + "orphan",
				"Containers": map[string]any{},
			})
		case req.Method == http.MethodDelete:
			deletePaths = append(deletePaths, req.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected Docker API request: "+req.Method+" "+req.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	host := strings.Replace(server.URL, "http://", "tcp://", 1)
	rt, err := New(context.Background(), host, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	require.Empty(t, deletePaths, "runtime construction must not clean resources before state restoration")
}

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

func TestInspectionConstructorDoesNotMutateDocker(t *testing.T) {
	var mutations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			mutations = append(mutations, req.Method+" "+req.URL.Path)
		}
		switch {
		case req.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(req.URL.Path, "/networks/create"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "network-a"})
		default:
			_ = json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer server.Close()
	rt, err := NewForInspection(context.Background(), strings.Replace(server.URL, "http://", "tcp://", 1), "")
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	require.Empty(t, mutations, "inspection must not create networks or clean stale objects")
}

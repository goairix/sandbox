package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceInspectionReturnsOnlyWritableWorkspaceBind(t *testing.T) {
	for _, tc := range []struct {
		name, mountType, destination string
		writable                     bool
		want                         string
	}{
		{"workspace_bind", "bind", "/workspace", true, "/srv/workspaces/team/a"},
		{"read_only", "bind", "/workspace", false, ""},
		{"volume", "volume", "/workspace", true, ""},
		{"other_path", "bind", "/other", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.47")
					_, _ = w.Write([]byte("OK"))
					return
				}
				require.Equal(t, http.MethodGet, req.Method)
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": "container-a", "State": map[string]bool{"Running": true},
					"Mounts": []any{map[string]any{"Type": tc.mountType, "Source": "/srv/workspaces/team/a", "Destination": tc.destination, "RW": tc.writable}}})
			}))
			defer server.Close()
			rt, err := NewForInspection(context.Background(), strings.Replace(server.URL, "http://", "tcp://", 1), "")
			require.NoError(t, err)
			defer func() { require.NoError(t, rt.Close()) }()
			info, err := rt.GetSandbox(context.Background(), "container-a")
			require.NoError(t, err)
			require.Equal(t, tc.want, info.WorkspaceHostPath)
		})
	}
}

func TestWorkspaceInspectionNormalizesMissingDockerRuntime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "container missing"})
	}))
	defer server.Close()
	rt, err := NewForInspection(context.Background(), strings.Replace(server.URL, "http://", "tcp://", 1), "")
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	_, err = rt.GetSandbox(context.Background(), "missing")
	require.ErrorIs(t, err, runtime.ErrNotFound)
}

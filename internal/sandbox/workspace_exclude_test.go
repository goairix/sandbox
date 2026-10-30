package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goairix/fs/driver/local"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
)

type excludeRuntime struct {
	*mockRuntime
	files    map[string]string
	uploaded map[string]string
}

func (r *excludeRuntime) DownloadDir(context.Context, string, string) (io.ReadCloser, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range r.files {
		if err := tw.WriteHeader(&tar.Header{Name: "workspace/" + name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(tw, content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

func (r *excludeRuntime) ExecPipe(_ context.Context, _ string, _ []string, input io.Reader) error {
	tr := tar.NewReader(input)
	r.uploaded = make(map[string]string)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		r.uploaded[h.Name] = string(data)
	}
}

var excludeLoggerOnce sync.Once

func excludeFixture(t *testing.T) (*Manager, *Sandbox, *excludeRuntime, string) {
	t.Helper()
	t.Chdir(t.TempDir())
	excludeLoggerOnce.Do(func() { require.NoError(t, logger.Init(&config.Config{})) })
	root := t.TempDir()
	filesystem, err := local.New(local.Config{RootPath: root})
	require.NoError(t, err)
	rt := &excludeRuntime{mockRuntime: newMockRuntime(), files: map[string]string{".agent/app/memory/today.md": "daily new"}}
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		var text strings.Builder
		for name := range rt.files {
			fmt.Fprintf(&text, "%s\tf\t0\n", name)
		}
		return &runtime.ExecResult{Stdout: text.String()}, nil
	}
	m := NewManager(rt, filesystem, &storage.FileSystemMeta{Provider: storage.ProviderMinIO}, ManagerConfig{})
	sb := &Sandbox{ID: "sandbox", RuntimeID: "runtime", State: StateRunning, Workspace: &WorkspaceInfo{RootPath: "user", SyncExclude: []string{".agent/memory.md"}}}
	scoped, err := storage.NewScopedFS(filesystem, "user")
	require.NoError(t, err)
	m.sandboxes[sb.ID] = sb
	m.workspaces[sb.ID] = scoped
	for name, content := range map[string]string{".agent/memory.md": "global new", ".agent/app/memory/today.md": "daily old", "removed.txt": "remove me", "skip.txt": "preserve me"} {
		p := filepath.Join(root, "user", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	return m, sb, rt, root
}

func TestWorkspaceSyncInheritsPersistedExcludes(t *testing.T) {
	for _, mode := range []string{"missing", "stale", "fallback"} {
		for _, operation := range []string{"manual", "request excludes", "automatic", "unmount", "destroy"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				m, sb, rt, root := excludeFixture(t)
				if mode != "missing" {
					rt.files[".agent/memory.md"] = "global stale"
				}
				if mode == "fallback" {
					rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
						return nil, errors.New("manifest unavailable")
					}
				}
				ctx := context.Background()
				switch operation {
				case "manual":
					require.NoError(t, m.SyncWorkspace(ctx, sb.ID, "from_container", nil))
				case "request excludes":
					require.NoError(t, m.SyncWorkspace(ctx, sb.ID, "from_container", []string{"skip.txt"}))
				case "automatic":
					m.autoSyncOnce()
				case "unmount":
					require.NoError(t, m.UnmountWorkspace(ctx, sb.ID))
				case "destroy":
					require.NoError(t, m.Destroy(ctx, sb.ID))
				}
				data, err := os.ReadFile(filepath.Join(root, "user/.agent/memory.md"))
				require.NoError(t, err)
				require.Equal(t, "global new", string(data))
				data, err = os.ReadFile(filepath.Join(root, "user/.agent/app/memory/today.md"))
				require.NoError(t, err)
				require.Equal(t, "daily new", string(data))
				if operation == "request excludes" {
					data, err = os.ReadFile(filepath.Join(root, "user/skip.txt"))
					require.NoError(t, err)
					require.Equal(t, "preserve me", string(data))
					require.Equal(t, []string{".agent/memory.md"}, sb.Workspace.SyncExclude)
				}
				if mode != "fallback" {
					_, err = os.Stat(filepath.Join(root, "user/removed.txt"))
					require.True(t, os.IsNotExist(err), "ordinary deletion must remain functional")
				}
			})
		}
	}
}

func TestWorkspaceSyncToContainerHonorsPersistedAndRequestExcludes(t *testing.T) {
	m, sb, rt, _ := excludeFixture(t)
	require.NoError(t, m.SyncWorkspace(context.Background(), sb.ID, "to_container", []string{"skip.txt"}))
	require.NotContains(t, rt.uploaded, ".agent/memory.md")
	require.NotContains(t, rt.uploaded, "skip.txt")
	require.Equal(t, "daily old", rt.uploaded[".agent/app/memory/today.md"])
	require.Contains(t, rt.uploaded, "removed.txt")
}

func TestWorkspaceMountHonorsAndCopiesExcludes(t *testing.T) {
	m, sb, rt, _ := excludeFixture(t)
	delete(m.workspaces, sb.ID)
	sb.Workspace = nil
	excludes := []string{".agent/memory.md"}
	require.NoError(t, m.MountWorkspace(context.Background(), sb.ID, "user", excludes))
	require.NotContains(t, rt.uploaded, ".agent/memory.md")
	require.Equal(t, "daily old", rt.uploaded[".agent/app/memory/today.md"])
	excludes[0] = "changed-by-caller"
	require.Equal(t, []string{".agent/memory.md"}, sb.Workspace.SyncExclude)
}

package sandbox_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandbox "github.com/goairix/sandbox-sdk-go"
)

func newSandboxTestServer(t *testing.T, sandboxID string) (*httptest.Server, *sandbox.Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: sandboxID, Mode: sandbox.ModeEphemeral, State: "running"})
	})
	mux.HandleFunc("/api/v1/sandboxes/"+sandboxID+"/exec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.ExecResponse{ExitCode: 0, Stdout: "hello\n"})
	})
	mux.HandleFunc("/api/v1/sandboxes/"+sandboxID, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, sandbox.NewClient(srv.URL, "test-key")
}

func TestNewSandbox(t *testing.T) {
	_, client := newSandboxTestServer(t, "sb-abc")
	sb, err := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})
	if err != nil {
		t.Fatalf("NewSandbox error: %v", err)
	}
	if sb.ID() != "sb-abc" {
		t.Errorf("ID = %q, want %q", sb.ID(), "sb-abc")
	}
}

func TestSandboxRun(t *testing.T) {
	_, client := newSandboxTestServer(t, "sb-abc")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})

	result, err := sb.Run(context.Background(), "python", `print("hello")`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello\n")
	}
}

func TestSandboxClose(t *testing.T) {
	_, client := newSandboxTestServer(t, "sb-abc")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})
	if err := sb.Close(context.Background()); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestClientRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/execute", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.ExecResponse{ExitCode: 0, Stdout: "42\n"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")

	result, err := client.Run(context.Background(), "python", `print(42)`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.Stdout != "42\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "42\n")
	}
}

func TestSandboxUploadDownload(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: "sb-xyz", Mode: sandbox.ModeEphemeral, State: "running"})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-xyz/files/upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.FileUploadResponse{Path: "/workspace/main.py", Size: 10})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-xyz/files/download", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "file content")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})

	if err := sb.UploadFile(context.Background(), "/workspace/main.py", strings.NewReader("print(1)")); err != nil {
		t.Fatalf("UploadFile error: %v", err)
	}
	rc, err := sb.DownloadFile(context.Background(), "/workspace/main.py")
	if err != nil {
		t.Fatalf("DownloadFile error: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "file content" {
		t.Errorf("content = %q, want %q", string(data), "file content")
	}
}

func TestSandboxNetworkOps(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: "sb-net", Mode: sandbox.ModeEphemeral, State: "running"})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-net/network", func(w http.ResponseWriter, r *http.Request) {
		var req sandbox.UpdateNetworkRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.UpdateNetworkResponse{Enabled: req.Enabled, Whitelist: req.Whitelist})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})

	if err := sb.EnableNetwork(context.Background(), []string{"api.openai.com"}); err != nil {
		t.Fatalf("EnableNetwork error: %v", err)
	}
	if err := sb.DisableNetwork(context.Background()); err != nil {
		t.Fatalf("DisableNetwork error: %v", err)
	}
}

func TestSandboxWorkspaceOps(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: "sb-ws", Mode: sandbox.ModeEphemeral, State: "running"})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-ws/workspace/mount", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.MountWorkspaceResponse{RootPath: "/data/user"})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-ws/workspace/unmount", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-ws/workspace/sync", func(w http.ResponseWriter, r *http.Request) {
		var req sandbox.SyncWorkspaceRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.SyncWorkspaceResponse{Direction: string(req.Direction), Message: "ok"})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-ws/workspace/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.WorkspaceInfoResponse{Mounted: true, RootPath: "/data/user"})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-ws/files/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.FileListResponse{Path: "/workspace", Files: []sandbox.FileInfo{{Name: "a.py", Path: "/workspace/a.py"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})

	if err := sb.MountWorkspace(context.Background(), "/data/user"); err != nil {
		t.Fatalf("MountWorkspace error: %v", err)
	}
	if resp, err := sb.Sync(context.Background()); err != nil || resp.Direction != "from_container" {
		t.Fatalf("Sync error: %v, direction: %s", err, resp.Direction)
	}
	if resp, err := sb.SyncTo(context.Background()); err != nil || resp.Direction != "to_container" {
		t.Fatalf("SyncTo error: %v, direction: %s", err, resp.Direction)
	}
	if info, err := sb.WorkspaceInfo(context.Background()); err != nil || !info.Mounted {
		t.Fatalf("WorkspaceInfo error: %v", err)
	}
	if files, err := sb.ListFiles(context.Background(), "/workspace"); err != nil || len(files.Files) != 1 {
		t.Fatalf("ListFiles error: %v", err)
	}
	if err := sb.UnmountWorkspace(context.Background()); err != nil {
		t.Fatalf("UnmountWorkspace error: %v", err)
	}
}

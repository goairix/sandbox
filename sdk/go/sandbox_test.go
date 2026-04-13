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

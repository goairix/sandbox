package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sandbox "github.com/goairix/sandbox-sdk-go"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *sandbox.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")
	return srv, client
}

func TestClientCreateSandbox(t *testing.T) {
	want := sandbox.SandboxResponse{
		ID:        "sb-123",
		Mode:      sandbox.ModeEphemeral,
		State:     "running",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing X-API-Key header")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want)
	})

	got, err := client.CreateSandbox(context.Background(), sandbox.CreateSandboxRequest{Mode: sandbox.ModeEphemeral})
	if err != nil {
		t.Fatalf("CreateSandbox error: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
}

func TestClientGetSandbox(t *testing.T) {
	want := sandbox.SandboxResponse{ID: "sb-456", Mode: sandbox.ModePersistent, State: "idle"}
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes/sb-456" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want)
	})

	got, err := client.GetSandbox(context.Background(), "sb-456")
	if err != nil {
		t.Fatalf("GetSandbox error: %v", err)
	}
	if got.ID != "sb-456" {
		t.Errorf("ID = %q, want %q", got.ID, "sb-456")
	}
}

func TestClientErrorResponse(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"code": "SANDBOX_NOT_FOUND", "message": "not found"})
	})

	_, err := client.GetSandbox(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

func TestExecStreamReturnsChunksAndTerminalExit(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes/sb/exec/stream" {
			t.Errorf("unexpected stream request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: stdout\ndata: {\"content\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: stderr\ndata: {\"content\":\"second\"}\n\n")
		_, _ = io.WriteString(w, "event: done\ndata: {\"exit_code\":4,\"elapsed\":0.1}\n\n")
	})
	stream, err := client.ExecStream(context.Background(), "sb", sandbox.ExecRequest{Language: "bash", Code: "echo test"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, want := range []sandbox.SSEEvent{
		{Type: sandbox.SSEEventStdout, Content: "first"},
		{Type: sandbox.SSEEventStderr, Content: "second"},
		{Type: sandbox.SSEEventDone, ExitCode: 4, Elapsed: 0.1},
	} {
		got, err := stream.Recv()
		if err != nil || got != want {
			t.Fatalf("event=%+v err=%v want=%+v", got, err, want)
		}
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal stream end = %v", err)
	}
}

func TestExecStreamDeliversOutputBeforeProcessCompletion(t *testing.T) {
	firstRead := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: stdout\ndata: {\"content\":\"ready\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-firstRead:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: done\ndata: {\"exit_code\":0}\n\n")
	}))
	defer server.Close()
	client := sandbox.NewClient(server.URL, "test-key")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.ExecStream(ctx, "sb", sandbox.ExecRequest{Language: "bash", Code: "printf ready; sleep 1"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Recv()
	if err != nil || first.Type != sandbox.SSEEventStdout || first.Content != "ready" {
		t.Fatalf("first event=%+v err=%v", first, err)
	}
	close(firstRead) // The command can finish only after the first chunk was consumed.
	last, err := stream.Recv()
	if err != nil || last.Type != sandbox.SSEEventDone || last.ExitCode != 0 {
		t.Fatalf("terminal event=%+v err=%v", last, err)
	}
}

func TestExecStreamReportsMalformedAndIncompleteSSE(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"missing terminal", "event: stdout\ndata: {\"content\":\"partial\"}\n\n", io.ErrUnexpectedEOF},
		{"malformed JSON", "event: stdout\ndata: not-json\n\n", sandbox.ErrMalformedStream},
		{"oversized line", "event: stdout\ndata: " + strings.Repeat("x", 2<<20) + "\n\n", sandbox.ErrMalformedStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			})
			stream, err := client.ExecStream(context.Background(), "sb", sandbox.ExecRequest{Language: "bash", Code: "true"})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			for {
				_, err := stream.Recv()
				if err != nil {
					if !errors.Is(err, tc.want) {
						t.Fatalf("stream error = %v, want %v", err, tc.want)
					}
					return
				}
			}
		})
	}
}

func TestExecStreamCloseCancelsSilentRequest(t *testing.T) {
	disconnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(disconnected)
	}))
	defer server.Close()
	client := sandbox.NewClient(server.URL, "test-key")
	stream, err := client.ExecStream(context.Background(), "sb", sandbox.ExecRequest{Language: "bash", Code: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("closing the stream did not cancel the server request")
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		t.Fatal(fmt.Errorf("closed stream returned %w", err))
	}
}

func TestExecStreamCallerCancellationIsNotEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := sandbox.NewClient(server.URL, "test-key")
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.ExecStream(ctx, "sb", sandbox.ExecRequest{Language: "bash", Code: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancel()
	if _, err := stream.Recv(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream returned %v, want context.Canceled", err)
	}
}

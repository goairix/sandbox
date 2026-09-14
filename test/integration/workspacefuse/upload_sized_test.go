package workspacefuse_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUploadSizedACK(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		size      int64
		wantError bool
	}{
		{"valid", 200, `{"path":"/workspace/large.bin","size":8192}`, 8192, false},
		{"zero", 200, `{"path":"/workspace/large.bin","size":0}`, 0, false},
		{"empty", 200, "", 8192, true},
		{"no_content", 204, "", 8192, true},
		{"created", 201, `{"path":"/workspace/large.bin","size":8192}`, 8192, true},
		{"missing_path", 200, `{"size":8192}`, 8192, true},
		{"missing_zero_size", 200, `{"path":"/workspace/large.bin"}`, 0, true},
		{"null_size", 200, `{"path":"/workspace/large.bin","size":null}`, 0, true},
		{"wrong_path", 200, `{"path":"/workspace/other.bin","size":8192}`, 8192, true},
		{"wrong_size", 200, `{"path":"/workspace/large.bin","size":8191}`, 8192, true},
		{"invalid", 200, "not-json", 8192, true},
		{"truncated", 200, `{"path":"/workspace/large.bin","size":8192`, 8192, true},
		{"trailing_value", 200, `{"path":"/workspace/large.bin","size":8192} {}`, 8192, true},
		{"trailing_junk", 200, `{"path":"/workspace/large.bin","size":8192} junk`, 8192, true},
		{"oversized", 200, `{"path":"/workspace/large.bin","size":8192,"padding":"` + strings.Repeat("x", 64<<10) + `"}`, 8192, true},
		{"rejected", 413, "too large", 8192, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("path") != "/workspace/large.bin" || r.Header.Get("X-Sandbox-File-Size") == "" {
					t.Error("missing upload path or declared size")
				}
				multipartReader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				part, err := multipartReader.NextPart()
				if err != nil {
					t.Error(err)
					return
				}
				count, err := io.Copy(io.Discard, part)
				if err != nil || count != tc.size {
					t.Errorf("file bytes=%d, want=%d, error=%v", count, tc.size, err)
				}
				if _, err := multipartReader.NextPart(); !errors.Is(err, io.EOF) {
					t.Errorf("multipart end: %v", err)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := uploadSized(ctx, &apiClient{baseURL: server.URL, client: server.Client()}, "sandbox", "/workspace/large.bin", tc.size)
			if (err != nil) != tc.wantError {
				t.Fatalf("upload error=%v, wantError=%t", err, tc.wantError)
			}
		})
	}
}

type uploadTransportFunc func(*http.Request) (*http.Response, error)

func (f uploadTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type uploadResponseBody struct {
	io.Reader
	readErr, closeErr error
	closed            bool
}

func (b *uploadResponseBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if errors.Is(err, io.EOF) && b.readErr != nil {
		return n, b.readErr
	}
	return n, err
}

func (b *uploadResponseBody) Close() error { b.closed = true; return b.closeErr }

func TestUploadSizedResponseErrors(t *testing.T) {
	for _, stage := range []string{"read", "close"} {
		t.Run(stage, func(t *testing.T) {
			sentinel := errors.New("response " + stage + " failed")
			body := &uploadResponseBody{Reader: strings.NewReader(`{"path":"/workspace/large.bin","size":32}`)}
			if stage == "read" {
				body.readErr = sentinel
			} else {
				body.closeErr = sentinel
			}
			client := &http.Client{Transport: uploadTransportFunc(func(r *http.Request) (*http.Response, error) {
				_, err := io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})}
			err := uploadSized(context.Background(), &apiClient{baseURL: "http://upload.invalid", client: client}, "sandbox", "/workspace/large.bin", 32)
			if !errors.Is(err, sentinel) {
				t.Fatalf("expected %s error, got %v", stage, err)
			}
			if !body.closed {
				t.Fatal("response body not closed")
			}
		})
	}
}

func TestUploadSizedEarlyExitClosesProducer(t *testing.T) {
	for _, stage := range []string{"request_error", "early_success", "early_rejection", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var requestBody io.ReadCloser
			client := &http.Client{Transport: uploadTransportFunc(func(r *http.Request) (*http.Response, error) {
				requestBody = r.Body
				if stage == "request_error" {
					return nil, errors.New("network failed")
				}
				if stage == "cancel" {
					cancel()
					return nil, ctx.Err()
				}
				status := 200
				if stage == "early_rejection" {
					status = 413
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"path":"/workspace/large.bin","size":1073741824}`)), Header: make(http.Header)}, nil
			})}
			err := uploadSized(ctx, &apiClient{baseURL: "http://upload.invalid", client: client}, "sandbox", "/workspace/large.bin", 1<<30)
			if requestBody == nil {
				t.Fatal("request was not dispatched")
			}
			defer func() { _ = requestBody.Close() }()
			if err == nil {
				t.Error("incomplete streaming upload reported success")
			}
			if stage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Errorf("expected cancellation, got %v", err)
			}
			readDone := make(chan error, 1)
			go func() {
				p := make([]byte, 1)
				n, readErr := requestBody.Read(p)
				if n > 0 {
					readErr = errors.New("producer still writing after helper returned")
				}
				readDone <- readErr
			}()
			select {
			case readErr := <-readDone:
				if !errors.Is(readErr, io.ErrClosedPipe) && !errors.Is(readErr, io.EOF) {
					t.Errorf("pipe not closed: %v", readErr)
				}
			case <-time.After(time.Second):
				t.Error("pipe reader still blocked after return")
			}
		})
	}
}

func TestUploadSizedInvalidRequest(t *testing.T) {
	err := uploadSized(context.Background(), &apiClient{baseURL: "://invalid", client: &http.Client{}}, "sandbox", "/workspace/large.bin", 1<<30)
	if err == nil {
		t.Fatal("invalid request accepted")
	}
}

func TestUploadSizedCancelDuringStreaming(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: uploadTransportFunc(func(r *http.Request) (*http.Response, error) {
		// Start consuming the pipe, then stop a producer blocked on its next
		// write. Cancellation must close it even before RoundTrip returns.
		buffer := make([]byte, 1)
		if _, err := r.Body.Read(buffer); err != nil {
			return nil, err
		}
		cancel()
		_, err := io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		return nil, err
	})}
	done := make(chan error, 1)
	go func() {
		done <- uploadSized(ctx, &apiClient{baseURL: "http://upload.invalid", client: client}, "sandbox", "/workspace/large.bin", 1<<40)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("streaming producer did not stop after cancellation")
	}
}

func TestUploadSizedNegativeSize(t *testing.T) {
	called := false
	client := &http.Client{Transport: uploadTransportFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected request")
	})}
	err := uploadSized(context.Background(), &apiClient{baseURL: "http://upload.invalid", client: client}, "sandbox", "/workspace/large.bin", -1)
	if err == nil || called {
		t.Fatalf("negative size must fail before dispatch: err=%v, dispatched=%t", err, called)
	}
}

func TestUploadSizedProducerJoin(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	produced := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- closeSizedUpload(reader, produced) }()
	// A blocked read is released only after closeSizedUpload closes the
	// reader. The lifecycle helper must then remain blocked until the
	// producer publishes its completion, not return at pipe closure.
	buffer := make([]byte, 1)
	readDone := make(chan error, 1)
	go func() { _, err := reader.Read(buffer); readDone <- err }()
	select {
	case err := <-readDone:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("pipe closure: %v", err)
		}
	case <-time.After(time.Second):
		produced <- nil
		t.Fatal("lifecycle helper did not close pipe")
	}
	select {
	case <-done:
		t.Fatal("returned before producer completed")
	case <-time.After(10 * time.Millisecond):
	}
	sentinel := errors.New("producer failed")
	produced <- sentinel
	select {
	case err := <-done:
		if !errors.Is(err, sentinel) {
			t.Errorf("producer error lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer result did not release lifecycle helper")
	}
}

func TestUploadSizedErrorsAreSafe(t *testing.T) {
	const sensitive = "test-credential-must-not-be-displayed"
	for _, stage := range []string{"create", "request", "read", "close", "json"} {
		t.Run(stage, func(t *testing.T) {
			sentinel := errors.New(sensitive + strings.Repeat("x", 128<<10))
			baseURL := "http://upload.invalid"
			if stage == "create" {
				baseURL = "://" + sensitive
			}
			client := &http.Client{Transport: uploadTransportFunc(func(r *http.Request) (*http.Response, error) {
				if stage == "request" {
					_ = r.Body.Close()
					return nil, sentinel
				}
				_, err := io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				if err != nil {
					return nil, err
				}
				body := &uploadResponseBody{Reader: strings.NewReader(`{"path":"/workspace/large.bin","size":0}`)}
				switch stage {
				case "read":
					body.readErr = sentinel
				case "close":
					body.closeErr = sentinel
				case "json":
					body.Reader = strings.NewReader(`{"path":"/workspace/large.bin","size":` + strings.Repeat("9", 4096) + `}`)
				}
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})}
			err := uploadSized(context.Background(), &apiClient{baseURL: baseURL, apiKey: sensitive, client: client}, "sandbox", "/workspace/large.bin", 0)
			if err == nil {
				t.Fatal("expected upload error")
			}
			if len(err.Error()) > 512 || strings.Contains(err.Error(), sensitive) {
				t.Fatalf("%s error was not bounded and sanitized", stage)
			}
			if stage == "request" || stage == "read" || stage == "close" {
				if !errors.Is(err, sentinel) {
					t.Fatal("sanitization lost structured error identity")
				}
			}
		})
	}
}

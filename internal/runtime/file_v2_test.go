package runtime

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFileV2TransportCloseJoins(t *testing.T) {
	cancelled := make(chan struct{})
	finished := make(chan error, 1)
	stream := NewFileOpStream(io.NopCloser(strings.NewReader("content")), func() { close(cancelled) }, finished)
	closed := make(chan error, 1)
	go func() { closed <- stream.Close() }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the helper")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before helper completion")
	default:
	}
	want := errors.New("helper failed")
	finished <- want
	select {
	case err := <-closed:
		if !errors.Is(err, want) {
			t.Fatalf("Close returned %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join the helper")
	}
	if err := stream.Close(); !errors.Is(err, want) {
		t.Fatalf("repeated Close returned %v", err)
	}
}

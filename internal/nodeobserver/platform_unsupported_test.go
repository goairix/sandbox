//go:build !linux || (!amd64 && !arm64)

package nodeobserver

import "testing"

func TestNodeObserverUnsupportedFailsClosed(t *testing.T) {
	if observer, err := New(Options{}); err == nil || observer != nil {
		t.Fatal("unsupported kernel created an observer")
	}
}

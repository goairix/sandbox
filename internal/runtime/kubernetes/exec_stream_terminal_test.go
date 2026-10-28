package kubernetes

import (
	"errors"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	kubeexec "k8s.io/client-go/util/exec"
)

func TestStreamExecTerminalDistinguishesProcessExitAndRuntimeFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want runtime.StreamEvent
	}{
		{"success", nil, runtime.StreamEvent{Type: runtime.StreamDone, Content: "0"}},
		{"nonzero exit", kubeexec.CodeExitError{Err: errors.New("command terminated with exit code 4"), Code: 4}, runtime.StreamEvent{Type: runtime.StreamDone, Content: "4"}},
		{"runtime failure", errors.New("transport closed"), runtime.StreamEvent{Type: runtime.StreamError, Content: "transport closed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamExecTerminal(tc.err); got != tc.want {
				t.Fatalf("terminal=%+v want=%+v", got, tc.want)
			}
		})
	}
}

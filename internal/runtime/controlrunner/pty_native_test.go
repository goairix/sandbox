//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"encoding/json"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"testing"
	"time"
)

func TestTask3NativePTY(t *testing.T) {
	f := newNativeFixture(t)
	for _, c := range []struct {
		name        string
		input, want []byte
	}{{"newline", []byte("hello\n"), []byte("hello\n")}, {"partial", []byte("partial"), []byte("partial")}, {"empty", nil, nil}, {"erase", []byte("ab\x7fc\n"), []byte("ac\n")}} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := f.start(t, controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "tty"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 10, Stdin: c.input, TTY: true}, 10*time.Second)
			out, stderr := drainExecution(t, e)
			if len(stderr) != 0 || !bytes.Contains(out, []byte("\nSTDERR_MARKER\n")) {
				t.Fatalf("PTY must merge stderr without output loss: %q separate%q", out, stderr)
			}
			line := bytes.SplitN(out, []byte("\n"), 2)[0]
			var got struct {
				PID   int     `json:"pid"`
				SID   int     `json:"sid"`
				TTY   [3]bool `json:"tty"`
				Stdin []byte  `json:"stdin"`
			}
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatalf("PTY output %q %v", out, err)
			}
			if got.TTY != [3]bool{true, true, true} || got.PID != got.SID || !bytes.Equal(got.Stdin, c.want) {
				t.Fatalf("actual PTY %+v wantinput%q", got, c.want)
			}
			t.Logf("actual PTY case=%s pid=sid=%d descriptors=%v canonical input=%q mergedoutput=%q", c.name, got.PID, got.TTY, got.Stdin, out)
		})
	}
}

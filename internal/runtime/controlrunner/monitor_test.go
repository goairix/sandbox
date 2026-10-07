package controlrunner

import (
	"bytes"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func TestMonitorRequestBindsBinaryInput(t *testing.T) {
	request := controlprotocol.ExecutionRequest{Argv: []string{"echo", "a b"}, Env: map[string]string{"PATH": "/signed"}, UID: 1000, GID: 1000, WorkDir: "/work", TimeoutSeconds: 3, Stdin: []byte{0, 255, 128, 10}}
	original := monitorStart{Request: request, AuthorityDeadlineNS: 123, CommandDeadlineNS: 456}
	var wire bytes.Buffer
	if err := writeMonitorRequest(&wire, original); err != nil {
		t.Fatal(err)
	}
	got, err := readMonitorRequest(&wire)
	if err != nil || !bytes.Equal(got.Request.Stdin, request.Stdin) || got.Request.Env["PATH"] != "/signed" || got.AuthorityDeadlineNS != 123 {
		t.Fatalf("request %+v %v", got, err)
	}
	if _, err = readMonitorRequest(bytes.NewReader([]byte{0, 0, 2, 0, 1})); err == nil {
		t.Fatal("oversized request accepted")
	}
}

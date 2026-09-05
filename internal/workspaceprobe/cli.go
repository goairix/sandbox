package workspaceprobe

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

// Run executes the complete public workspace-probe CLI grammar.
func Run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runCLI(New(), argv, stdin, stdout, stderr)
}

func runCLI(probe *Probe, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if !fuseprotocol.AllowedProbeCLI(argv) {
		writeCLIError(stderr, "unsupported workspace-probe command")
		return 2
	}
	if argv[1] == "self-check" {
		if err := probe.SelfCheck(); err != nil {
			writeCLIError(stderr, err.Error())
			return 1
		}
		return 0
	}
	generation, err := strconv.ParseInt(argv[5], 10, 64)
	if err != nil || generation <= 0 {
		writeCLIError(stderr, "invalid generation")
		return 2
	}
	runtimeUID := argv[3]
	var status fuseprotocol.ProbeStatus
	switch argv[1] {
	case "write-read-delete":
		status, err = probe.WriteReadDelete(runtimeUID, generation)
	case "quiesce":
		status, err = probe.Quiesce(runtimeUID, generation)
	case "resume":
		var input []byte
		input, err = io.ReadAll(io.LimitReader(stdin, fuseprotocol.MaxJSONBytes+1))
		if err == nil && len(input) > fuseprotocol.MaxJSONBytes {
			err = fmt.Errorf("workspace probe input exceeds limit")
		}
		var request fuseprotocol.ProbeResumeRequest
		if err == nil {
			err = fuseprotocol.DecodeExact(input, &request)
		}
		if err == nil && (request.Version != fuseprotocol.Version || request.RuntimeUID != runtimeUID || request.Generation != generation) {
			err = fmt.Errorf("resume envelope does not match argv")
		}
		if err == nil {
			err = probe.Resume(resumeRequest{RuntimeUID: request.RuntimeUID, Generation: request.Generation, Token: request.Token})
		}
		if err == nil {
			status = successStatus(runtimeUID, generation, "")
		}
	}
	if err != nil {
		writeCLIError(stderr, err.Error())
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(status); err != nil {
		writeCLIError(stderr, "write workspace probe status")
		return 1
	}
	return 0
}

func writeCLIError(stderr io.Writer, message string) {
	message = strings.ReplaceAll(message, "\n", " ")
	_, _ = fmt.Fprintln(stderr, message)
}

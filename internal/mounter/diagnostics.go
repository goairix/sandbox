package mounter

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const diagnosticLimit = 16 << 10

type DiagnosticError struct {
	Code     string
	ExitCode int
}

func (e *DiagnosticError) Error() string {
	if e == nil {
		return "workspace mounter failure"
	}
	return "workspace mounter failure: " + e.Code
}

func DiagnosticCode(err error) string {
	var diagnostic *DiagnosticError
	if !errors.As(err, &diagnostic) || diagnostic == nil {
		return ""
	}
	switch diagnostic.Code {
	case fuseprotocol.MounterErrorFusePermission,
		fuseprotocol.MounterErrorEndpointDNS,
		fuseprotocol.MounterErrorEndpointTLS,
		fuseprotocol.MounterErrorStorageAuth,
		fuseprotocol.MounterErrorStorageBucket,
		fuseprotocol.MounterErrorS3FSExited:
		return diagnostic.Code
	default:
		return ""
	}
}

type boundedDiagnosticBuffer struct {
	buffer bytes.Buffer
}

func newDiagnosticBuffer() *boundedDiagnosticBuffer {
	return &boundedDiagnosticBuffer{}
}

func (b *boundedDiagnosticBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := diagnosticLimit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.buffer.Write(value)
	}
	return original, nil
}

func (b *boundedDiagnosticBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

func (b *boundedDiagnosticBuffer) Clear() {
	raw := b.buffer.Bytes()
	for index := range raw {
		raw[index] = 0
	}
	b.buffer.Reset()
}

func classifyS3FSDiagnostic(processErr error, stderr []byte) error {
	if processErr == nil {
		return nil
	}
	message := strings.ToLower(string(stderr))
	code := fuseprotocol.MounterErrorS3FSExited
	switch {
	case containsAny(message,
		"failed to open /dev/fuse",
		"fuse: device not found",
		"fusermount: permission denied",
		"operation not permitted",
	):
		code = fuseprotocol.MounterErrorFusePermission
	case containsAny(message,
		"could not resolve host",
		"couldn't resolve host",
		"name or service not known",
		"temporary failure in name resolution",
		"getaddrinfo",
	):
		code = fuseprotocol.MounterErrorEndpointDNS
	case containsAny(message,
		"ssl certificate problem",
		"certificate verify failed",
		"peer certificate cannot be authenticated",
		"unable to get local issuer certificate",
	):
		code = fuseprotocol.MounterErrorEndpointTLS
	case containsAny(message,
		"accessdenied",
		"access denied",
		"invalidaccesskeyid",
		"signaturedoesnotmatch",
		"response code 403",
		"http code 403",
		"403 forbidden",
	):
		code = fuseprotocol.MounterErrorStorageAuth
	case containsAny(message,
		"specified bucket does not exist",
		"nosuchbucket",
		"response code 404",
		"http code 404",
		"404 not found",
	):
		code = fuseprotocol.MounterErrorStorageBucket
	}
	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(processErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return &DiagnosticError{Code: code, ExitCode: exitCode}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

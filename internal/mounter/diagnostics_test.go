package mounter

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoundedDiagnosticBufferDiscardsOverflowWithoutShortWrite(t *testing.T) {
	buffer := newDiagnosticBuffer()
	input := []byte(strings.Repeat("x", diagnosticLimit+1024))

	written, err := buffer.Write(input)

	require.NoError(t, err)
	assert.Equal(t, len(input), written)
	assert.Len(t, buffer.Bytes(), diagnosticLimit)
	buffer.Clear()
	assert.Empty(t, buffer.Bytes())
}

func TestClassifyS3FSDiagnosticReturnsOnlySafeCodes(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{name: "fuse permission", stderr: "fuse: failed to open /dev/fuse: Operation not permitted", want: "fuse-permission"},
		{name: "dns", stderr: "Could not resolve host: minio.internal", want: "endpoint-dns"},
		{name: "tls", stderr: "SSL certificate problem: unable to get local issuer certificate", want: "endpoint-tls"},
		{name: "auth", stderr: "HTTP response code 403 AccessDenied AKIA-DO-NOT-LEAK secret-value", want: "storage-auth"},
		{name: "bucket", stderr: "The specified bucket does not exist", want: "storage-bucket"},
		{name: "unknown", stderr: "request failed https://example.invalid/?X-Amz-Signature=secret-value", want: "s3fs-exited"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyS3FSDiagnostic(errors.New("exit status 1"), []byte(test.stderr))
			assert.Equal(t, test.want, DiagnosticCode(err))
			assert.NotContains(t, err.Error(), "AKIA-DO-NOT-LEAK")
			assert.NotContains(t, err.Error(), "secret-value")
			assert.NotContains(t, err.Error(), "example.invalid")
		})
	}
}

func TestClassifyS3FSDiagnosticPreservesSuccessfulExit(t *testing.T) {
	assert.NoError(t, classifyS3FSDiagnostic(nil, []byte("ignored diagnostic")))
}

func TestDiagnosticTokenUsesExactAllowListedGrammar(t *testing.T) {
	for _, code := range []string{
		"fuse-permission", "endpoint-dns", "endpoint-tls",
		"storage-auth", "storage-bucket", "s3fs-exited",
	} {
		token, ok := FormatDiagnosticToken(&DiagnosticError{Code: code, ExitCode: 1})
		require.True(t, ok)
		assert.Equal(t, "workspace-mounter-error:"+code+"\n", token)
		parsed, ok := ParseDiagnosticToken([]byte(token))
		require.True(t, ok)
		assert.Equal(t, code, parsed)
	}
}

func TestDiagnosticTokenRejectsUntrustedText(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("workspace-mounter-error:rejected\n"),
		[]byte("workspace-mounter-error:unknown\n"),
		[]byte("prefix workspace-mounter-error:endpoint-tls\n"),
		[]byte("workspace-mounter-error:endpoint-tls"),
		[]byte("workspace-mounter-error:endpoint-tls\nAKIA-DO-NOT-LEAK\n"),
		[]byte(strings.Repeat("x", diagnosticLimit+1)),
	} {
		code, ok := ParseDiagnosticToken(raw)
		assert.False(t, ok, string(raw))
		assert.Empty(t, code)
	}
	token, ok := FormatDiagnosticToken(errors.New("AKIA-DO-NOT-LEAK"))
	assert.False(t, ok)
	assert.Empty(t, token)
}

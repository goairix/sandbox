package kubernetes

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestWriteSizedTarRejectsInvalidBodyLengths(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		body string
	}{
		{name: "negative", size: -1},
		{name: "short", size: 5, body: "four"},
		{name: "excess", size: 4, body: "extra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeSizedTar(io.Discard, "a.txt", 0o644, 1000, 1000, tc.size, strings.NewReader(tc.body))
			require.ErrorIs(t, err, runtime.ErrInvalidUploadSize)
		})
	}
}

func TestUploadFileCommandRejectsDirectoryDestination(t *testing.T) {
	command := uploadFileCommand("/workspace/.sandbox-upload-temp", "/workspace/target")
	assert.Contains(t, command, "test ! -d")
	assert.Contains(t, command, "mv -f --")
}

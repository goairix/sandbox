package docker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/docker/docker/pkg/stdcopy"
)

// copyDockerExecFrames keeps Docker's stdout/stderr frame boundaries while
// treating an incomplete header or body as a transport failure. StdCopy
// silently accepts EOF inside a frame, which would turn a broken execution
// stream into a successful done event.
func copyDockerExecFrames(stdout, stderr io.Writer, source io.Reader) error {
	var header [8]byte
	for {
		_, err := io.ReadFull(source, header[:])
		if errors.Is(err, io.EOF) {
			return nil // EOF is valid only between complete frames.
		}
		if err != nil {
			return fmt.Errorf("read Docker exec frame header: %w", err)
		}
		var target io.Writer
		switch stdcopy.StdType(header[0]) {
		case stdcopy.Stdout:
			target = stdout
		case stdcopy.Stderr:
			target = stderr
		case stdcopy.Systemerr:
			return errors.New("docker daemon reported an execution stream error")
		default:
			return fmt.Errorf("unsupported Docker exec stream type %d", header[0])
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err := io.CopyN(target, source, size); err != nil {
			return fmt.Errorf("read Docker exec frame body: %w", err)
		}
	}
}

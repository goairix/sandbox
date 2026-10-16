package sandboxfiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const MaxDownloadInputBytes = 16 << 10
const MaxDownloadBytes int64 = 2 << 30

type DownloadRequest struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"max_bytes"`
}

func ValidateDownload(req DownloadRequest) error {
	if req.MaxBytes < 1 || req.MaxBytes > MaxDownloadBytes {
		return failureCode("invalid_request")
	}
	return Validate(Request{Operation: "download", Path: req.Path})
}

func DecodeDownloadRequest(wire []byte) (DownloadRequest, error) {
	var req DownloadRequest
	if len(wire) > MaxDownloadInputBytes || decodeStrict(wire, &req) != nil {
		return req, failureCode("invalid_request")
	}
	canonical, err := json.Marshal(req)
	if err != nil || !bytes.Equal(wire, canonical) {
		return DownloadRequest{}, failureCode("invalid_request")
	}
	return req, ValidateDownload(req)
}

func StreamDownload(req DownloadRequest, output io.Writer) error {
	if err := ValidateDownload(req); err != nil {
		return err
	}
	base, _ := rootPath(req.Path)
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	err = streamDownloadAt(req, root, output)
	return errors.Join(err, root.Close())
}

// Tests can pin a private root without adding a host-root production parameter.
func streamDownloadAt(req DownloadRequest, root *os.Root, output io.Writer) (err error) {
	if err := ValidateDownload(req); err != nil {
		return err
	}
	_, rel := rootPath(req.Path)
	file, err := root.OpenFile(rel, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return failureCode("unsupported_type")
	}
	if info.Size() < 0 || info.Size() > req.MaxBytes {
		return failureCode("limit_exceeded")
	}
	original := info.Size()
	remaining := original
	var buffer [32 << 10]byte
	for remaining > 0 {
		next := int64(len(buffer))
		if remaining < next {
			next = remaining
		}
		n, readErr := io.ReadFull(file, buffer[:int(next)])
		if readErr != nil {
			return readErr
		}
		written, writeErr := output.Write(buffer[:n])
		if writeErr != nil {
			return writeErr
		}
		if written != n {
			return io.ErrShortWrite
		}
		remaining -= int64(n)
	}
	n, err := file.Read(buffer[:1])
	if n != 0 || err != io.EOF {
		return errors.New("download source changed")
	}
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != original {
		return errors.New("download source changed")
	}
	return nil
}

func downloadFailureCode(code string) bool {
	switch code {
	case "invalid_request", "not_found", "limit_exceeded", "unsupported_type", "permission_denied", "io_error":
		return true
	}
	return false
}

func EncodeDownloadFailure(err error) []byte {
	code := failure(err).Error.Code
	if !downloadFailureCode(code) {
		code = "io_error"
	}
	wire, _ := json.Marshal(Failure{Code: code})
	return append(wire, '\n')
}

func DecodeDownloadFailure(wire []byte) (Failure, error) {
	var result Failure
	if len(wire) > 256 || decodeStrict(wire, &result) != nil || !downloadFailureCode(result.Code) {
		return Failure{}, errors.New("invalid download failure")
	}
	return result, nil
}

package redisbootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type BootstrapFilePaths struct{ Cluster, Registration, PublicKeys string }
type BootstrapFiles struct {
	Cluster      ClusterState
	Registration *BootstrapRegistration
	PublicKeys   [3]ed25519.PublicKey
}

var errBootstrapFiles = errors.New("bootstrap control files are unconfirmed")

// ReadBootstrapFiles reads trusted Kubernetes projections (including ..data
// symlinks), not untrusted PVC files. Repeated bytes and strict cross-document
// checks reject a mixed projection update; they are not a control-plane CAS.
func ReadBootstrapFiles(ctx context.Context, paths BootstrapFilePaths) (out BootstrapFiles, resultErr error) {
	if ctx == nil || !validBootstrapPaths(paths) {
		return out, errBootstrapFiles
	}
	defer func() {
		if resultErr != nil {
			out = BootstrapFiles{}
			if ctx.Err() != nil {
				resultErr = ctx.Err()
			} else {
				resultErr = errBootstrapFiles
			}
		}
	}()
	var first [3][]byte
	for pass := range 2 {
		var data [3][]byte
		var err error
		data[0], err = readBootstrapProjection(ctx, paths.Cluster, maximumStateBytes)
		if err != nil {
			return out, err
		}
		data[1], err = readBootstrapProjection(ctx, paths.Registration, maximumStateBytes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return out, err
		}
		data[2], err = readBootstrapProjection(ctx, paths.PublicKeys, 1024)
		if err != nil {
			return out, err
		}
		if pass == 0 {
			first = data
			continue
		}
		for i := range data {
			if !bytes.Equal(first[i], data[i]) {
				return out, errBootstrapFiles
			}
		}
		out.Cluster, err = ParseClusterState(data[0])
		if err != nil {
			return out, err
		}
		out.PublicKeys, err = ParseMemberPublicKeys(data[2])
		if err != nil {
			return out, err
		}
		if data[1] == nil {
			if out.Cluster.Phase != Pending {
				return out, errBootstrapFiles
			}
		} else {
			r, err := ParseBootstrapRegistration(data[1])
			if err != nil || r.Cluster != out.Cluster {
				return out, errBootstrapFiles
			}
			digest, err := PublicKeySetDigest(out.PublicKeys)
			if err != nil || digest != r.KeyDigest {
				return out, errBootstrapFiles
			}
			out.Registration = &r
		}
	}
	return out, ctx.Err()
}

// WaitBootstrapInitialized only reads the initialization gate. The caller must
// separately use its authenticated Redis HA/ACK contract before business writes.
func WaitBootstrapInitialized(ctx context.Context, paths BootstrapFilePaths) error {
	if ctx == nil || !validBootstrapPaths(paths) {
		return errBootstrapFiles
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for {
		files, err := ReadBootstrapFiles(ctx, paths)
		if err == nil && files.Registration != nil && BusinessWritersAllowed(&files.Cluster) {
			return nil
		}
		if err := waitBootstrapPoll(ctx); err != nil {
			return err
		}
	}
}

func validBootstrapPaths(paths BootstrapFilePaths) bool {
	seen := map[string]bool{}
	for _, path := range []string{paths.Cluster, paths.Registration, paths.PublicKeys} {
		clean := filepath.Clean(path)
		if !filepath.IsAbs(path) || clean == string(filepath.Separator) || seen[clean] {
			return false
		}
		seen[clean] = true
	}
	return true
}

func readBootstrapProjection(ctx context.Context, path string, limit int64) (data []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, errBootstrapFiles
	}
	defer func() {
		if file.Close() != nil {
			data = nil
			resultErr = errBootstrapFiles
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errBootstrapFiles
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, errBootstrapFiles
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func waitBootstrapPoll(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

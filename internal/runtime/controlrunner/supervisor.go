package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
)

func nilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && strings.ToLower(s) == s
}

func NewSupervisor(o SupervisorOptions) (*Supervisor, error) {
	if o.MaxActive == 0 {
		o.MaxActive = 4
	}
	if o.UID == 0 || o.UID > 2147483647 || o.GID == 0 || o.GID > 2147483647 || o.MaxActive > 64 || o.Verifier == nil || nilValue(o.Clock) || !validHash(o.ContractDigest) || !filepath.IsAbs(o.JournalDirectory) || filepath.Clean(o.JournalDirectory) != o.JournalDirectory || !filepath.IsAbs(o.Executable) || filepath.Clean(o.Executable) != o.Executable || o.JournalMaxBytes < 0 || o.JournalMaxBytes > 256*1024*1024 {
		return nil, ErrInvalidConfiguration
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return nil, launcher.ErrUnsupported
	}
	if os.Getpid() != 1 || o.Kernel == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := o.Kernel.ValidateCurrent(); err != nil {
		return nil, err
	}
	if o.Kernel.Snapshot().PID != 1 {
		return nil, ErrInvalidConfiguration
	}
	executable, err := inspectExecutable(o.Executable)
	if err != nil {
		return nil, err
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	boot, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	s := &Supervisor{pid: os.Getpid(), options: o, key: key, executable: executable, active: make(map[string]*Execution)}
	s.birth = controlprotocol.BirthContext{BootID: boot.String(), RuntimePublicKey: public, UID: o.UID, GID: o.GID, NetworkAllowed: o.NetworkAllowed, ContractDigest: o.ContractDigest}
	s.self = s
	return s, nil
}
func (s *Supervisor) validateReceiver() error {
	if s == nil || s.self != s || s.pid != 1 || os.Getpid() != s.pid {
		return ErrUnavailable
	}
	return nil
}
func (s *Supervisor) Birth() controlprotocol.BirthContext {
	if s == nil || s.self != s {
		return controlprotocol.BirthContext{}
	}
	b := s.birth
	b.RuntimePublicKey = append([]byte(nil), b.RuntimePublicKey...)
	return b
}
func observeControlled(ctx context.Context, clock controlprotocol.AuthorityClock) (time.Time, error) {
	if nilValue(ctx) || nilValue(clock) {
		return time.Time{}, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	o, err := clock.Observe(bounded)
	elapsed := time.Since(start)
	if err != nil {
		return time.Time{}, err
	}
	if err = bounded.Err(); err != nil {
		return time.Time{}, err
	}
	if o.UTC.IsZero() || o.UTC.Location() != time.UTC || o.UTC.Year() < 1 || o.UTC.Year() > 9999 || o.Uncertainty < 0 || o.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-o.Uncertainty {
		return time.Time{}, fmt.Errorf("invalid or late authority clock")
	}
	return o.UTC.Add(elapsed), nil
}

package controlrunner

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrolledSupervisorAdmission(t *testing.T) {
	s := &Supervisor{enrollmentOptions: &NodeEnrollmentOptions{}}
	a, fresh, err := s.reserveEnrollment(context.Background(), "activation")
	require.NoError(t, err)
	require.True(t, fresh)
	require.False(t, s.admission)
	got, fresh, err := s.reserveEnrollment(context.Background(), "activation")
	require.NoError(t, err)
	require.False(t, fresh)
	require.Same(t, a, got)
	_, _, err = s.reserveEnrollment(context.Background(), "different")
	require.Error(t, err)
	s.mu.Lock()
	s.enrollmentSealed = true
	s.mu.Unlock()
	_, _, err = s.reserveEnrollment(context.Background(), "activation")
	require.Error(t, err)
	a.cancel()
	a.lifeCancel()
}

func TestEnrolledSupervisorCloseJoin(t *testing.T) {
	s := &Supervisor{enrollmentOptions: &NodeEnrollmentOptions{}}
	a, _, err := s.reserveEnrollment(context.Background(), "activation")
	require.NoError(t, err)
	directory, err := os.MkdirTemp("/tmp", "enrolled-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	path := filepath.Join(directory, "observer.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	defer l.Close()
	conn, err := net.DialUnix("unix", nil, l.Addr().(*net.UnixAddr))
	require.NoError(t, err)
	peer, err := l.AcceptUnix()
	require.NoError(t, err)
	defer peer.Close()
	s.mu.Lock()
	a.conn = conn
	s.mu.Unlock()
	readDone := make(chan struct{})
	go func() { var b [1]byte; _, _ = conn.Read(b[:]); close(readDone) }()
	joined := make(chan struct{})
	go func() { s.joinEnrollment(a); close(joined) }()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("original socket read did not return")
	}
	select {
	case <-joined:
		t.Fatal("cancel counted as owner join")
	default:
	}
	close(a.done)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("owner did not join")
	}
}

func TestEnrolledSupervisorOptions(t *testing.T) {
	_, err := NewEnrolledSupervisor(SupervisorOptions{}, NodeEnrollmentOptions{})
	require.ErrorIs(t, err, ErrInvalidConfiguration)
}

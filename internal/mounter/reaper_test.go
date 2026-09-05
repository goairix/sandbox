package mounter

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

func callDockerReaper(t *testing.T, server *DockerReaperServer, request fuseprotocol.DockerReaperRequest) fuseprotocol.DockerReaperResponse {
	t.Helper()
	left, right := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.handle(context.Background(), left)
		_ = left.Close()
		close(done)
	}()
	require.NoError(t, writeReaperFrame(right, request))
	var response fuseprotocol.DockerReaperResponse
	require.NoError(t, readReaperFrame(right, &response))
	_ = right.Close()
	<-done
	return response
}

func TestDockerReaperRegistersOnlyExactPeerChild(t *testing.T) {
	waited := make(chan reaperProcessIdentity, 1)
	server := &DockerReaperServer{
		PeerIdentity: func(net.Conn) (int, int, error) { return 50, 1000, nil },
		Process: func(int) (reaperProcessIdentity, error) {
			return reaperProcessIdentity{PID: 51, PPID: 50, UID: 1000, StartTime: 42}, nil
		},
		WaitExact: func(_ context.Context, identity reaperProcessIdentity) error { waited <- identity; return nil },
	}
	left, right := net.Pipe()
	done := make(chan struct{})
	go func() { server.handle(context.Background(), left); _ = left.Close(); close(done) }()
	require.NoError(t, writeReaperFrame(right, fuseprotocol.DockerReaperRequest{Version: 1, Command: "register", PID: 51, UID: 1000, StartTime: 42}))
	var response fuseprotocol.DockerReaperResponse
	require.NoError(t, readReaperFrame(right, &response))
	assert.True(t, response.Accepted)
	assert.Equal(t, 51, (<-waited).PID)
	_ = right.Close()
	<-done
}

func TestDockerReaperRejectsPIDNotOwnedByPeer(t *testing.T) {
	server := &DockerReaperServer{
		PeerIdentity: func(net.Conn) (int, int, error) { return 50, 1000, nil },
		Process: func(int) (reaperProcessIdentity, error) {
			return reaperProcessIdentity{PID: 51, PPID: 49, UID: 1000, StartTime: 42}, nil
		},
		WaitExact: func(context.Context, reaperProcessIdentity) error {
			t.Fatal("must not reap unverified process")
			return nil
		},
	}
	left, right := net.Pipe()
	go func() { server.handle(context.Background(), left); _ = left.Close() }()
	require.NoError(t, writeReaperFrame(right, fuseprotocol.DockerReaperRequest{Version: 1, Command: "register", PID: 51, UID: 1000, StartTime: 42}))
	var response fuseprotocol.DockerReaperResponse
	require.NoError(t, readReaperFrame(right, &response))
	assert.False(t, response.Accepted)
	_ = right.Close()
}

func TestDockerReaperRegistrationLimitIsGlobalAndReleasedAfterWait(t *testing.T) {
	release := make(chan struct{})
	waitStarted := make(chan struct{}, 1)
	server := &DockerReaperServer{
		PeerIdentity: func(net.Conn) (int, int, error) { return 50, 1000, nil },
		Process: func(pid int) (reaperProcessIdentity, error) {
			return reaperProcessIdentity{PID: pid, PPID: 50, UID: 1000, StartTime: uint64(pid)}, nil
		},
		WaitExact: func(_ context.Context, _ reaperProcessIdentity) error {
			waitStarted <- struct{}{}
			<-release
			return nil
		},
	}

	first := callDockerReaper(t, server, fuseprotocol.DockerReaperRequest{Version: 1, Command: "register", PID: 51, UID: 1000, StartTime: 51})
	require.True(t, first.Accepted)
	<-waitStarted
	second := callDockerReaper(t, server, fuseprotocol.DockerReaperRequest{Version: 1, Command: "register", PID: 52, UID: 1000, StartTime: 52})
	assert.False(t, second.Accepted)
	assert.Equal(t, "registration_limit", second.ErrorCode)

	close(release)
	require.Eventually(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		return len(server.registered) == 0
	}, time.Second, time.Millisecond)
	third := callDockerReaper(t, server, fuseprotocol.DockerReaperRequest{Version: 1, Command: "register", PID: 52, UID: 1000, StartTime: 52})
	assert.True(t, third.Accepted)
}

func TestWaitDockerReaperChildReturnsWhenIdentityGoneAndWaitHasNoChild(t *testing.T) {
	lookupCalls := 0
	err := waitDockerReaperChild(
		context.Background(),
		reaperProcessIdentity{PID: 51, UID: 1000, StartTime: 42},
		func(int) (reaperProcessIdentity, error) {
			lookupCalls++
			return reaperProcessIdentity{}, os.ErrNotExist
		},
		func(int) (bool, error) { return false, syscall.ECHILD },
	)
	require.NoError(t, err)
	assert.Equal(t, 1, lookupCalls)
}

func TestWaitDockerReaperChildFailsClosedWhenIdentityCannotBeRead(t *testing.T) {
	sentinel := errors.New("proc read denied")
	err := waitDockerReaperChild(
		context.Background(),
		reaperProcessIdentity{PID: 51, UID: 1000, StartTime: 42},
		func(int) (reaperProcessIdentity, error) { return reaperProcessIdentity{}, sentinel },
		func(int) (bool, error) { return false, syscall.ECHILD },
	)
	require.ErrorIs(t, err, sentinel)
}

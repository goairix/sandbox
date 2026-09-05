package mounter

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const maxDockerReaperRegistrations = 1

type reaperProcessIdentity struct {
	PID, PPID, UID int
	StartTime      uint64
}

// DockerReaperServer is a second, narrowly scoped PID 1 socket. It never
// executes commands and never calls wait4(-1); it accepts only one exact
// UID-1000 child identity at a time and reaps that PID after orphan adoption.
type DockerReaperServer struct {
	PeerIdentity func(net.Conn) (int, int, error)
	Process      func(int) (reaperProcessIdentity, error)
	WaitExact    func(context.Context, reaperProcessIdentity) error
	mu           sync.Mutex
	registered   map[int]uint64
}

func (s *DockerReaperServer) Serve(ctx context.Context) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("Docker reaper server must be PID 1")
	}
	listener, err := net.Listen("unix", fuseprotocol.DockerReaperSocket)
	if err != nil {
		return fmt.Errorf("listen for Docker reaper handshake: %w", err)
	}
	defer listener.Close()
	go func() { <-ctx.Done(); _ = listener.Close() }()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() { defer connection.Close(); s.handle(ctx, connection) }()
	}
}

func (s *DockerReaperServer) handle(ctx context.Context, connection net.Conn) {
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	peer := s.PeerIdentity
	if peer == nil {
		peer = dockerReaperPeerIdentity
	}
	peerPID, peerUID, err := peer(connection)
	if err != nil || peerPID <= 1 || peerUID != 1000 {
		return
	}
	var request fuseprotocol.DockerReaperRequest
	if readReaperFrame(connection, &request) != nil || request.Version != fuseprotocol.Version || request.UID != 1000 {
		return
	}
	switch request.Command {
	case "ping":
		if request.PID != 0 || request.StartTime != 0 {
			return
		}
		_ = writeReaperFrame(connection, fuseprotocol.DockerReaperResponse{Version: fuseprotocol.Version, Accepted: true, ErrorCode: ""})
	case "register":
		lookup := s.Process
		if lookup == nil {
			lookup = dockerReaperProcessIdentity
		}
		identity, lookupErr := lookup(request.PID)
		if lookupErr != nil || request.PID <= 1 || request.StartTime == 0 || identity.PID != request.PID || identity.PPID != peerPID || identity.UID != 1000 || identity.StartTime != request.StartTime {
			_ = writeReaperFrame(connection, fuseprotocol.DockerReaperResponse{Version: fuseprotocol.Version, Accepted: false, ErrorCode: "unverified_child"})
			return
		}
		s.mu.Lock()
		if s.registered == nil {
			s.registered = make(map[int]uint64)
		}
		_, duplicate := s.registered[identity.PID]
		registrationLimit := !duplicate && len(s.registered) >= maxDockerReaperRegistrations
		if !duplicate && !registrationLimit {
			s.registered[identity.PID] = identity.StartTime
		}
		s.mu.Unlock()
		if duplicate {
			_ = writeReaperFrame(connection, fuseprotocol.DockerReaperResponse{Version: fuseprotocol.Version, Accepted: false, ErrorCode: "duplicate_child"})
			return
		}
		if registrationLimit {
			_ = writeReaperFrame(connection, fuseprotocol.DockerReaperResponse{Version: fuseprotocol.Version, Accepted: false, ErrorCode: "registration_limit"})
			return
		}
		wait := s.WaitExact
		if wait == nil {
			wait = dockerWaitExactChild
		}
		go func() {
			_ = wait(ctx, identity)
			s.mu.Lock()
			delete(s.registered, identity.PID)
			s.mu.Unlock()
		}()
		_ = writeReaperFrame(connection, fuseprotocol.DockerReaperResponse{Version: fuseprotocol.Version, Accepted: true, ErrorCode: ""})
	}
}

func waitDockerReaperChild(
	ctx context.Context,
	expected reaperProcessIdentity,
	lookup func(int) (reaperProcessIdentity, error),
	waitNoHang func(int) (bool, error),
) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, identityErr := lookup(expected.PID)
		identityGone := errors.Is(identityErr, os.ErrNotExist)
		if identityErr != nil && !identityGone {
			return fmt.Errorf("registered Docker broker identity cannot be verified: %w", identityErr)
		}
		if identityErr == nil && (current.UID != expected.UID || current.StartTime != expected.StartTime) {
			return fmt.Errorf("registered Docker broker PID was reused")
		}
		reaped, waitErr := waitNoHang(expected.PID)
		if reaped {
			return nil
		}
		noChild := errors.Is(waitErr, syscall.ECHILD)
		if waitErr != nil && !noChild {
			return waitErr
		}
		if identityGone && noChild {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func writeReaperFrame(writer io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > fuseprotocol.MaxJSONBytes {
		return fmt.Errorf("invalid Docker reaper frame")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	if _, err := writer.Write(size[:]); err != nil {
		return err
	}
	_, err = writer.Write(raw)
	return err
}

func readReaperFrame(reader io.Reader, value any) error {
	var size [4]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > fuseprotocol.MaxJSONBytes {
		return fmt.Errorf("invalid Docker reaper frame size")
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return err
	}
	return fuseprotocol.DecodeExact(raw, value)
}

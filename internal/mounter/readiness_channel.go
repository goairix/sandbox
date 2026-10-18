package mounter

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/google/uuid"
)

const (
	// The directory is mounted read-only into the trusted helper, rather than
	// pinning a socket inode that prevents an Observer from rebinding on restart.
	readinessObserverSocket   = "/run/sandbox-node-observer/mounter-health/roothealth.sock"
	readinessChannelIOTimeout = time.Second
	readinessChannelRetry     = 250 * time.Millisecond
)

// ReadinessChannel connects the trusted native helper to the node's root-only
// Observer socket. Its public strings locate independently authenticated Pod
// and enrollment state; they grant neither readiness nor management authority.
// Serve owns one connection at a time and joins its IO before returning. It
// never probes a mount or dispatches authorize, flush, or shutdown operations.
type ReadinessChannel struct {
	Supervisor *Supervisor

	// SocketPath must be the compiled roothealth.sock path. The remaining
	// immutable deployment locators are compared with original enrollment.
	SocketPath     string
	PodName        string
	NodeUID        string
	NodeBootID     string
	ContractDigest string
}

func (c *ReadinessChannel) validate() error {
	if c == nil || c.Supervisor == nil || c.SocketPath != readinessObserverSocket || len(c.PodName) == 0 || len(c.PodName) > 128 || strings.ContainsAny(c.PodName, "/\\\x00\r\n") {
		return fmt.Errorf("invalid mounter readiness channel configuration")
	}
	for _, value := range []string{c.NodeUID, c.NodeBootID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return fmt.Errorf("invalid mounter readiness node identity")
		}
	}
	digest, err := hex.DecodeString(c.ContractDigest)
	if err != nil || len(digest) != 32 || strings.ToLower(c.ContractDigest) != c.ContractDigest {
		return fmt.Errorf("invalid mounter readiness contract digest")
	}
	return nil
}

// Serve requires the original native PID1 owner and a concrete kernel root
// peer. There is deliberately no caller-supplied peer-verification callback.
// Startup precedes launcher enrollment, so unavailable/unregistered peers are
// retried at a bounded rate. An absent channel never changes local mount state.
func (c *ReadinessChannel) Serve(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("missing mounter readiness context")
	}
	if err := c.validate(); err != nil {
		return err
	}
	if err := c.Supervisor.checkNativeBinding(); err != nil {
		return err
	}
	c.Supervisor.mu.Lock()
	if c.Supervisor.nativeOwner == nil {
		c.Supervisor.mu.Unlock()
		return fmt.Errorf("mounter readiness channel requires original native owner")
	}
	err := c.Supervisor.validateNativeLocked()
	c.Supervisor.mu.Unlock()
	if err != nil {
		return err
	}

	for ctx.Err() == nil {
		// Inspect the directory on each reconnect; an existing symlink or a
		// writable non-root socket is never accepted as a replacement endpoint.
		info, statErr := os.Lstat(filepath.Dir(c.SocketPath))
		if statErr == nil && info.IsDir() && info.Mode().Perm() == 0700 && ownedByRoot(info) {
			socket, socketErr := os.Lstat(c.SocketPath)
			if socketErr == nil && socket.Mode()&os.ModeSocket != 0 && socket.Mode().Perm() == 0600 && ownedByRoot(socket) {
				dial, cancel := context.WithTimeout(ctx, readinessChannelIOTimeout)
				conn, dialErr := (&net.Dialer{}).DialContext(dial, "unix", c.SocketPath)
				cancel()
				if dialErr == nil {
					unixConn, ok := conn.(*net.UnixConn)
					if ok && defaultVerifyRootPeer(unixConn) == nil {
						_ = c.serveConnection(ctx, unixConn)
					}
					_ = conn.Close()
				}
			}
		}
		timer := time.NewTimer(readinessChannelRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}

// serveConnection is only the finite framed transport component. The public
// entry above authenticates the original native owner and root peer first.
// Keeping it separate permits real Unix host tests without forging a kernel
// owner or adding a production authentication callback.
func (c *ReadinessChannel) serveConnection(ctx context.Context, conn *net.UnixConn) (result error) {
	if ctx == nil || conn == nil || c == nil || c.Supervisor == nil {
		return fmt.Errorf("missing readiness channel owner")
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		_ = conn.Close()
	})
	defer func() {
		_ = conn.Close()
		if !stop() {
			<-closed
		}
		if ctx.Err() != nil {
			result = ctx.Err()
		}
	}()
	c.Supervisor.mu.Lock()
	uid := c.Supervisor.bootstrap.RuntimeUID
	c.Supervisor.mu.Unlock()
	hello, err := json.Marshal(struct {
		Version        int    `json:"version"`
		RuntimeUID     string `json:"runtime_uid"`
		PodName        string `json:"pod_name"`
		NodeUID        string `json:"node_uid"`
		NodeBootID     string `json:"node_boot_id"`
		ContractDigest string `json:"contract_digest"`
	}{2, uid, c.PodName, c.NodeUID, c.NodeBootID, c.ContractDigest})
	if err != nil {
		return err
	}
	request, err := marshalBounded(fuseprotocol.SocketRequest{Version: fuseprotocol.Version, Command: "register-readiness", Input: hello})
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(readinessChannelIOTimeout)); err != nil {
		return err
	}
	if err := writeFrame(conn, request); err != nil {
		return err
	}
	wire, err := readFrame(conn)
	if err != nil {
		return err
	}
	var response fuseprotocol.SocketResponse
	var ack struct {
		Version  int  `json:"version"`
		Accepted bool `json:"accepted"`
	}
	if fuseprotocol.DecodeExact(wire, &response) != nil || response.Version != fuseprotocol.Version || !response.OK || response.ErrorCode != "" || fuseprotocol.DecodeExact(response.Output, &ack) != nil || ack.Version != 2 || !ack.Accepted {
		return fmt.Errorf("readiness registration refused")
	}

	for {
		// Idle connections consume no sampler. Once any frame byte arrives,
		// the rest of that single request and reply have a one-second budget.
		// Cancellation closes the original connection and joins this loop.
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return err
		}
		var first [1]byte
		if _, err := io.ReadFull(conn, first[:]); err != nil {
			return err
		}
		if err := conn.SetDeadline(time.Now().Add(readinessChannelIOTimeout)); err != nil {
			return err
		}
		wire, err := readFrame(io.MultiReader(bytes.NewReader(first[:]), conn))
		if err != nil {
			return err
		}
		var request fuseprotocol.SocketRequest
		var version struct {
			Version int `json:"version"`
		}
		if fuseprotocol.DecodeExact(wire, &request) != nil || request.Version != fuseprotocol.Version || request.Command != "health-ready" || fuseprotocol.DecodeExact(request.Input, &version) != nil || version.Version != 2 {
			if err := writeResponse(conn, nil, fuseprotocol.MounterErrorRejected); err != nil {
				return err
			}
			continue
		}
		status, err := c.Supervisor.residentReadiness(ctx)
		if err != nil {
			if err := writeResponse(conn, nil, fuseprotocol.MounterErrorRejected); err != nil {
				return err
			}
			continue
		}
		output, err := marshalBounded(status)
		if err != nil {
			return err
		}
		if err := writeResponse(conn, output, ""); err != nil {
			return err
		}
	}
}

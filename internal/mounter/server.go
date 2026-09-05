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
	"path/filepath"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const (
	defaultIOTimeout = 10 * time.Second
	errCodeRejected  = "rejected"
)

type Server struct {
	Supervisor         *Supervisor
	SocketPath         string
	ExpectedRuntimeUID string
	VerifyPeer         func(*net.UnixConn) error
	IOTimeout          time.Duration
}

func (s *Server) Serve(ctx context.Context) error {
	if s.Supervisor == nil || !filepath.IsAbs(s.SocketPath) || filepath.Base(s.SocketPath) != "control.sock" {
		return fmt.Errorf("mounter server configuration is invalid")
	}
	if err := secureDirectory(filepath.Dir(s.SocketPath), 0o700); err != nil {
		return fmt.Errorf("validate control socket directory: %w", err)
	}
	if _, err := os.Lstat(s.SocketPath); !os.IsNotExist(err) {
		return fmt.Errorf("control socket path is already present")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.SocketPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on control socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(s.SocketPath)
	}()
	if err := os.Chmod(s.SocketPath, 0o600); err != nil {
		return fmt.Errorf("secure control socket: %w", err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		<-serveCtx.Done()
		_ = listener.Close()
	}()
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			if serveCtx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept control connection: %w", acceptErr)
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			s.handle(serveCtx, connection)
		}()
	}
}

func (s *Server) handle(serverContext context.Context, connection *net.UnixConn) {
	defer connection.Close()
	timeout := s.IOTimeout
	if timeout <= 0 || timeout > time.Minute {
		timeout = defaultIOTimeout
	}
	_ = connection.SetReadDeadline(time.Now().Add(timeout))
	verify := s.VerifyPeer
	if verify == nil {
		verify = defaultVerifyRootPeer
	}
	if err := verify(connection); err != nil {
		return
	}
	raw, err := readFrame(connection)
	if err != nil {
		_ = connection.SetWriteDeadline(time.Now().Add(timeout))
		_ = writeResponse(connection, nil, errCodeRejected)
		return
	}
	_ = connection.SetReadDeadline(time.Time{})
	_ = connection.SetWriteDeadline(time.Now().Add(timeout))
	var request fuseprotocol.SocketRequest
	if err := fuseprotocol.DecodeExact(raw, &request); err != nil || request.Version != fuseprotocol.Version || len(request.Input) == 0 {
		_ = writeResponse(connection, nil, errCodeRejected)
		return
	}
	connectionContext, disconnect := context.WithCancel(serverContext)
	defer disconnect()
	go func() {
		var trailing [1]byte
		_, _ = connection.Read(trailing[:])
		disconnect()
	}()
	operationContext, cancel := context.WithTimeout(connectionContext, controlOperationTimeout(request.Command))
	defer cancel()
	output, dispatchErr := s.dispatch(operationContext, request.Command, request.Input)
	_ = connection.SetWriteDeadline(time.Now().Add(timeout))
	if dispatchErr != nil {
		_ = writeResponse(connection, nil, errCodeRejected)
		return
	}
	_ = writeResponse(connection, output, "")
}

func controlOperationTimeout(command string) time.Duration {
	switch command {
	case "health-ready", "flush", "shutdown", "shutdown-best-effort":
		return 2*time.Hour + 5*time.Second
	default:
		return 30 * time.Second
	}
}

func (s *Server) dispatch(ctx context.Context, command string, input []byte) ([]byte, error) {
	switch command {
	case "bootstrap":
		var request fuseprotocol.BootstrapConfig
		if err := fuseprotocol.Decode(input, &request); err != nil {
			return nil, err
		}
		expectedUID := s.ExpectedRuntimeUID
		if expectedUID == "" {
			expectedUID = request.RuntimeUID
		}
		if err := s.Supervisor.Bootstrap(ctx, request, expectedUID); err != nil {
			return nil, err
		}
		return marshalBounded(fuseprotocol.ControlAck{Version: fuseprotocol.Version, Accepted: true, RuntimeUID: expectedUID})
	case "authorize":
		var request fuseprotocol.AuthorizeRequest
		if err := fuseprotocol.DecodeExact(input, &request); err != nil {
			return nil, err
		}
		if err := s.Supervisor.Authorize(ctx, request); err != nil {
			return nil, err
		}
		return marshalBounded(fuseprotocol.ControlAck{Version: fuseprotocol.Version, Accepted: true, RuntimeUID: request.RuntimeUID, Generation: request.LeaseGeneration})
	case "health-prepared", "health-ready":
		if string(input) != "{}" {
			return nil, fmt.Errorf("health request payload is invalid")
		}
		var status fuseprotocol.MounterStatus
		var err error
		if terminal := s.Supervisor.Status(); terminal.RestartDetected {
			return marshalBounded(terminal)
		}
		if command == "health-prepared" {
			status, err = s.Supervisor.PreparedStatus()
		} else {
			status, err = s.Supervisor.ReadyStatus(ctx)
		}
		if err != nil {
			return nil, err
		}
		return marshalBounded(status)
	case "flush":
		var request fuseprotocol.ControlRequest
		if err := fuseprotocol.DecodeExact(input, &request); err != nil {
			return nil, err
		}
		ack, err := s.Supervisor.Flush(ctx, request)
		if err != nil {
			return nil, err
		}
		return marshalBounded(ack)
	case "shutdown":
		var request fuseprotocol.ControlRequest
		if err := fuseprotocol.DecodeExact(input, &request); err != nil {
			return nil, err
		}
		ack, err := s.Supervisor.Shutdown(ctx, &request)
		if err != nil {
			return nil, err
		}
		return marshalBounded(ack)
	case "shutdown-best-effort":
		if string(input) != "{}" {
			return nil, fmt.Errorf("best-effort shutdown payload is invalid")
		}
		_, err := s.Supervisor.Shutdown(ctx, nil)
		return []byte(`{}`), err
	default:
		return nil, fmt.Errorf("unknown mounter control command")
	}
}

func marshalBounded(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > fuseprotocol.MaxJSONBytes {
		return nil, fmt.Errorf("encode bounded mounter response")
	}
	return raw, nil
}

func writeResponse(writer io.Writer, output []byte, errorCode string) error {
	if len(output) == 0 {
		output = []byte(`{}`)
	}
	raw, err := marshalBounded(fuseprotocol.SocketResponse{Version: fuseprotocol.Version, OK: errorCode == "", Output: json.RawMessage(output), ErrorCode: errorCode})
	if err != nil {
		return err
	}
	return writeFrame(writer, raw)
}

func readFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, fmt.Errorf("read control frame header")
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > fuseprotocol.MaxJSONBytes {
		return nil, fmt.Errorf("control frame has invalid size")
	}
	raw := make([]byte, int(size))
	if _, err := io.ReadFull(reader, raw); err != nil {
		return nil, fmt.Errorf("read control frame body")
	}
	return raw, nil
}

func writeFrame(writer io.Writer, raw []byte) error {
	if len(raw) == 0 || len(raw) > fuseprotocol.MaxJSONBytes {
		return fmt.Errorf("control frame has invalid size")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if err := writeAll(writer, header[:]); err != nil {
		return fmt.Errorf("write control frame header")
	}
	if err := writeAll(writer, raw); err != nil {
		return fmt.Errorf("write control frame body")
	}
	return nil
}

func writeAll(writer io.Writer, raw []byte) error {
	for len(raw) != 0 {
		written, err := writer.Write(raw)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(raw) {
			return io.ErrShortWrite
		}
		raw = raw[written:]
	}
	return nil
}

type Client struct {
	SocketPath string
	IOTimeout  time.Duration
}

func (c Client) Do(ctx context.Context, command string, input []byte) ([]byte, error) {
	if !filepath.IsAbs(c.SocketPath) || command == "" || len(input) == 0 {
		return nil, fmt.Errorf("mounter client request is invalid")
	}
	request, err := marshalBounded(fuseprotocol.SocketRequest{Version: fuseprotocol.Version, Command: command, Input: json.RawMessage(append([]byte(nil), input...))})
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to mounter supervisor")
	}
	defer connection.Close()
	timeout := c.IOTimeout
	if timeout <= 0 || timeout > time.Minute {
		timeout = defaultIOTimeout
	}
	_ = connection.SetWriteDeadline(time.Now().Add(timeout))
	if err := writeFrame(connection, request); err != nil {
		return nil, err
	}
	_ = connection.SetWriteDeadline(time.Time{})
	readDeadline := time.Now().Add(2*time.Hour + time.Minute)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(readDeadline) {
		readDeadline = deadline
	}
	_ = connection.SetReadDeadline(readDeadline)
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-cancelWatch:
		}
	}()
	defer close(cancelWatch)
	raw, err := readFrame(connection)
	if err != nil {
		return nil, err
	}
	var response fuseprotocol.SocketResponse
	if err := fuseprotocol.DecodeExact(raw, &response); err != nil || response.Version != fuseprotocol.Version {
		return nil, fmt.Errorf("mounter supervisor response is invalid")
	}
	if !response.OK || response.ErrorCode != "" {
		return nil, fmt.Errorf("mounter supervisor rejected request")
	}
	return append([]byte(nil), response.Output...), nil
}

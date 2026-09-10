package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/docker/docker/api/types/container"
	dnetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/mounter"
)

const dockerPublicUser = "1000:1000"

var (
	ErrInvalidControlCommand = errors.New("invalid Docker workspace control command")
	ErrInvalidProbeCommand   = errors.New("invalid Docker workspace probe command")
)

// execControl is the only Docker exec path allowed to run as root. Its argv is
// a closed, versioned grammar and its input/output are bounded JSON frames.
func (r *Runtime) execControl(ctx context.Context, containerID string, argv []string, stdin []byte) ([]byte, error) {
	if !fuseprotocol.AllowedDockerMounterCommand(argv) {
		return nil, ErrInvalidControlCommand
	}
	return r.execFixed(ctx, containerID, "root", argv, stdin)
}

// setupFUSERoute is a private closed control operation. Docker grants these
// fixed execs the route capability required to block the Docker host gateway
// and install the policy gateway; public UID-1000 execs cannot retain the
// root-only effective capability.
func (r *Runtime) setupFUSERoute(ctx context.Context, containerID, pairNetworkID, gatewayIP string) error {
	ip := net.ParseIP(gatewayIP)
	if ip == nil || ip.To4() == nil || ip.String() != gatewayIP {
		return ErrInvalidControlCommand
	}
	network, err := r.cli.NetworkInspect(ctx, pairNetworkID, dnetwork.InspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect Docker FUSE pair network route: %w", err)
	}
	hostGatewayIP, err := dockerBridgeGatewayIPv4(network)
	if err != nil || hostGatewayIP == gatewayIP {
		return ErrInvalidControlCommand
	}
	if err := r.runFixedRouteControl(ctx, containerID, []string{"/usr/sbin/ip", "route", "replace", "blackhole", hostGatewayIP + "/32"}); err != nil {
		return fmt.Errorf("block Docker bridge host gateway: %w", err)
	}
	if err := r.runFixedRouteControl(ctx, containerID, []string{"/usr/sbin/ip", "route", "replace", "default", "via", gatewayIP}); err != nil {
		return fmt.Errorf("install Docker FUSE policy gateway: %w", err)
	}
	return nil
}

func dockerBridgeGatewayIPv4(network dnetwork.Inspect) (string, error) {
	var result string
	for _, item := range network.IPAM.Config {
		ip := net.ParseIP(item.Gateway)
		if ip == nil || ip.To4() == nil || ip.String() != item.Gateway {
			continue
		}
		if result != "" {
			return "", fmt.Errorf("Docker FUSE pair network has multiple IPv4 gateways")
		}
		result = item.Gateway
	}
	if result == "" {
		return "", fmt.Errorf("Docker FUSE pair network has no canonical IPv4 gateway")
	}
	return result, nil
}

func (r *Runtime) runFixedRouteControl(ctx context.Context, containerID string, argv []string) error {
	execResponse, err := r.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: argv, User: "root", WorkingDir: "/",
	})
	if err != nil {
		return fmt.Errorf("create fixed Docker FUSE route control: %w", err)
	}
	if err := r.cli.ContainerExecStart(ctx, execResponse.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("start fixed Docker FUSE route control: %w", err)
	}
	if err := waitExecDone(ctx, r.cli, execResponse.ID); err != nil {
		return fmt.Errorf("fixed Docker FUSE route control failed: %w", err)
	}
	return nil
}

func (r *Runtime) execProbe(ctx context.Context, containerID string, argv []string, stdin []byte) ([]byte, error) {
	if !fuseprotocol.AllowedProbeCommand(argv) {
		return nil, ErrInvalidProbeCommand
	}
	return r.execFixed(ctx, containerID, dockerPublicUser, argv, stdin)
}

func (r *Runtime) execFixed(ctx context.Context, containerID, user string, argv []string, stdin []byte) ([]byte, error) {
	if len(stdin) > fuseprotocol.MaxJSONBytes {
		return nil, fmt.Errorf("workspace control input exceeds limit")
	}
	execResponse, err := r.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: append([]string(nil), argv...), User: user, AttachStdin: len(stdin) != 0,
		AttachStdout: true, AttachStderr: true, WorkingDir: "/",
	})
	if err != nil {
		return nil, fmt.Errorf("create Docker workspace control exec: %w", err)
	}
	attach, err := r.cli.ContainerExecAttach(ctx, execResponse.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attach Docker workspace control exec: %w", err)
	}
	defer attach.Close()
	stopWatch := watchDockerAttachContext(ctx, attach.Close)
	defer stopWatch()
	if len(stdin) != 0 {
		if _, err := io.Copy(attach.Conn, bytes.NewReader(stdin)); err != nil {
			return nil, fmt.Errorf("write Docker workspace control input: %w", err)
		}
	}
	if err := attach.CloseWrite(); err != nil {
		return nil, fmt.Errorf("close Docker workspace control input: %w", err)
	}
	stdout := &limitedControlBuffer{limit: fuseprotocol.MaxJSONBytes}
	stderr := &limitedControlBuffer{limit: fuseprotocol.MaxJSONBytes}
	if _, err := stdcopy.StdCopy(stdout, stderr, attach.Reader); err != nil {
		return nil, fmt.Errorf("read Docker workspace control output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("Docker workspace control output exceeds limit")
	}
	inspect, err := r.cli.ContainerExecInspect(ctx, execResponse.ID)
	if err != nil {
		return nil, fmt.Errorf("inspect Docker workspace control exec: %w", err)
	}
	if inspect.ExitCode != 0 {
		return nil, dockerControlExecError(stderr.Bytes())
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func dockerControlExecError(stderr []byte) error {
	if code, ok := mounter.ParseDiagnosticToken(stderr); ok {
		return fmt.Errorf("Docker workspace control command failed: %s", code)
	}
	return fmt.Errorf("Docker workspace control command failed")
}

func watchDockerAttachContext(ctx context.Context, closeAttach func()) func() {
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	go func() {
		select {
		case <-ctx.Done():
			closeAttach()
		case <-done:
		}
	}()
	return stop
}

type limitedControlBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedControlBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = b.overflow || original > 0
		return original, nil
	}
	if len(data) > remaining {
		b.overflow = true
		data = data[:remaining]
	}
	_, _ = b.Buffer.Write(data)
	return original, nil
}

func decodeControlAck(raw []byte) (fuseprotocol.ControlAck, error) {
	var value fuseprotocol.ControlAck
	if err := fuseprotocol.DecodeExact(raw, &value); err != nil || value.Version != fuseprotocol.Version {
		return fuseprotocol.ControlAck{}, fmt.Errorf("invalid Docker workspace control acknowledgement")
	}
	return value, nil
}

func decodeMounterStatus(raw []byte) (fuseprotocol.MounterStatus, error) {
	var value fuseprotocol.MounterStatus
	if err := fuseprotocol.DecodeExact(raw, &value); err != nil || value.Version != fuseprotocol.Version {
		return fuseprotocol.MounterStatus{}, fmt.Errorf("invalid Docker mounter status")
	}
	return value, nil
}

func decodeProbeStatus(raw []byte) (fuseprotocol.ProbeStatus, error) {
	var value fuseprotocol.ProbeStatus
	if err := fuseprotocol.DecodeExact(raw, &value); err != nil || value.Version != fuseprotocol.Version {
		return fuseprotocol.ProbeStatus{}, fmt.Errorf("invalid Docker workspace probe status")
	}
	return value, nil
}

func decodeShutdownAck(raw []byte) (fuseprotocol.ShutdownAck, error) {
	var value fuseprotocol.ShutdownAck
	if err := fuseprotocol.DecodeExact(raw, &value); err != nil || value.Version != fuseprotocol.Version {
		return fuseprotocol.ShutdownAck{}, fmt.Errorf("invalid Docker shutdown acknowledgement")
	}
	return value, nil
}

func marshalControl(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > fuseprotocol.MaxJSONBytes {
		return nil, fmt.Errorf("encode Docker workspace control request")
	}
	return raw, nil
}

func dockerProbeArgv(command string, refID string, generation int64, resume bool) []string {
	argv := []string{fuseprotocol.ProbeBinary, command, "--runtime-uid", refID, "--generation", fmt.Sprintf("%d", generation)}
	if resume {
		argv = append(argv, "--token-stdin")
	}
	return argv
}

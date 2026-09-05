package workspaceprobe

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const (
	brokerEnvironment = "WORKSPACE_PROBE_INTERNAL_BROKER_V1"
	brokerStateFD     = 3
	brokerReadyFD     = 4
)

type localBroker struct {
	processes  processTable
	random     io.Reader
	executable func() (string, error)
	reaper     func() error
}

type hybridBroker struct {
	pid1      *pid1Broker
	local     *localBroker
	usePID1   bool
	pid1Probe bool
}

type brokerLaunch struct {
	Version    int               `json:"version"`
	RuntimeUID string            `json:"runtime_uid"`
	Generation int64             `json:"generation"`
	Processes  []processIdentity `json:"processes"`
	SocketID   string            `json:"socket_id"`
	Secret     string            `json:"secret"`
}

type brokerResumeWire struct {
	Version    int    `json:"version"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
	Secret     string `json:"secret"`
}

type brokerResponse struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
}

func newLocalBroker(processes processTable, random io.Reader) *localBroker {
	return &localBroker{processes: processes, random: random, executable: os.Executable, reaper: requirePID1Reaper}
}

func newHybridBroker(processes processTable, random io.Reader) *hybridBroker {
	return &hybridBroker{pid1: &pid1Broker{processes: processes}, local: newLocalBroker(processes, random)}
}

func (b *hybridBroker) protectedProcess() (*processIdentity, error) {
	process, available, err := b.pid1.protectedProcess()
	b.pid1Probe = true
	b.usePID1 = available
	return process, err
}

func (b *hybridBroker) start(state brokerState) (string, error) {
	if !b.pid1Probe {
		return "", fmt.Errorf("pid 1 broker must be probed before registration")
	}
	if b.usePID1 {
		return b.pid1.start(state)
	}
	return b.local.start(state)
}

func (b *hybridBroker) resume(request resumeRequest) error {
	if strings.HasPrefix(request.Token, "p1.") {
		return b.pid1.resume(request)
	}
	return b.local.resume(request)
}

func (b *localBroker) start(state brokerState) (string, error) {
	if err := b.reaper(); err != nil {
		return "", fmt.Errorf("sandbox pid 1 cannot reap quiesce broker: %w", err)
	}
	if len(state.Processes) == 0 {
		// A broker is still required: it binds the token to the live quiesce
		// generation and provides replay protection even in an idle sandbox.
	}
	socketBytes := make([]byte, 16)
	secretBytes := make([]byte, 32)
	if _, err := io.ReadFull(b.random, socketBytes); err != nil {
		return "", err
	}
	if _, err := io.ReadFull(b.random, secretBytes); err != nil {
		wipe(socketBytes)
		return "", err
	}
	defer wipe(socketBytes)
	defer wipe(secretBytes)
	socketID := base64.RawURLEncoding.EncodeToString(socketBytes)
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	launch := brokerLaunch{
		Version: fuseprotocol.Version, RuntimeUID: state.RuntimeUID, Generation: state.Generation,
		Processes: append([]processIdentity(nil), state.Processes...), SocketID: socketID, Secret: secret,
	}
	encoded, err := json.Marshal(launch)
	if err != nil || len(encoded) > fuseprotocol.MaxJSONBytes {
		return "", fmt.Errorf("encode quiesce broker state")
	}
	stateReader, stateWriter, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer stateReader.Close()
	defer stateWriter.Close()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	executable, err := b.executable()
	if err != nil {
		return "", err
	}
	command := localBrokerCommand(executable, state)
	command.Env = append(os.Environ(), brokerEnvironment+"=1")
	command.ExtraFiles = []*os.File{stateReader, readyWriter}
	command.SysProcAttr = detachedProcessAttributes()
	if err := command.Start(); err != nil {
		return "", err
	}
	_ = stateReader.Close()
	_ = readyWriter.Close()
	if err := writeAll(stateWriter, encoded); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return "", err
	}
	_ = stateWriter.Close()
	if err := readyReader.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return "", err
	}
	ready := make([]byte, 1)
	if _, err := io.ReadFull(readyReader, ready); err != nil || ready[0] != 1 {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return "", fmt.Errorf("quiesce broker did not become ready")
	}
	if err := command.Process.Release(); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return "", err
	}
	return "v1." + socketID + "." + secret, nil
}

func localBrokerCommand(executable string, state brokerState) *exec.Cmd {
	command := exec.Command(executable, "quiesce", "--runtime-uid", state.RuntimeUID, "--generation", strconv.FormatInt(state.Generation, 10))
	command.Dir = "/"
	return command
}

func (b *localBroker) resume(request resumeRequest) error {
	socketID, secret, err := parseOpaqueToken(request.Token)
	if err != nil {
		return ErrInvalidToken
	}
	connection, err := net.DialTimeout("unix", abstractSocket(socketID), 2*time.Second)
	if err != nil {
		return ErrInvalidToken
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	wire := brokerResumeWire{Version: fuseprotocol.Version, RuntimeUID: request.RuntimeUID, Generation: request.Generation, Secret: secret}
	if err := writeBrokerFrame(connection, wire); err != nil {
		return ErrInvalidToken
	}
	var response brokerResponse
	if err := readBrokerFrame(connection, &response); err != nil || response.Version != fuseprotocol.Version || !response.OK {
		return ErrInvalidToken
	}
	return nil
}

// MaybeRunBroker switches a re-executed quiesce process into the private
// in-memory token broker. The public argv remains valid, but without the two
// inherited pipes this path cannot be invoked by a sandbox workload.
func MaybeRunBroker() (bool, int) {
	if os.Getenv(brokerEnvironment) != "1" {
		return false, 0
	}
	if os.Geteuid() != requiredUID || os.Getegid() != requiredGID {
		return true, 1
	}
	stateFile := os.NewFile(brokerStateFD, "workspace-probe-broker-state")
	readyFile := os.NewFile(brokerReadyFD, "workspace-probe-broker-ready")
	if stateFile == nil || readyFile == nil {
		return true, 1
	}
	defer stateFile.Close()
	defer readyFile.Close()
	var launch brokerLaunch
	if err := decodeLimitedJSON(stateFile, &launch); err != nil || validateBrokerLaunch(launch) != nil {
		return true, 1
	}
	if err := disableProcessDump(); err != nil {
		return true, 1
	}
	if err := os.Chdir("/"); err != nil {
		return true, 1
	}
	listener, err := net.Listen("unix", abstractSocket(launch.SocketID))
	if err != nil {
		return true, 1
	}
	defer listener.Close()
	if _, err := readyFile.Write([]byte{1}); err != nil {
		return true, 1
	}
	_ = stateFile.Close()
	_ = readyFile.Close()
	return true, serveBroker(listener, launch, procFS{root: "/proc"})
}

func serveBroker(listener net.Listener, launch brokerLaunch, processes processTable) int {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return 1
		}
		consumed, ok := handleBrokerConnection(connection, launch, processes)
		_ = connection.Close()
		if consumed {
			if ok {
				return 0
			}
			return 1
		}
	}
}

func handleBrokerConnection(connection net.Conn, launch brokerLaunch, processes processTable) (consumed, ok bool) {
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	var request brokerResumeWire
	if err := readBrokerFrame(connection, &request); err != nil {
		writeBrokerResponse(connection, false)
		return false, false
	}
	if request.Version != fuseprotocol.Version || request.RuntimeUID != launch.RuntimeUID || request.Generation != launch.Generation ||
		subtle.ConstantTimeCompare([]byte(request.Secret), []byte(launch.Secret)) != 1 {
		writeBrokerResponse(connection, false)
		return false, false
	}
	// The valid secret is consumed before touching processes. A verification or
	// resume failure cannot be retried with a potentially stale PID identity.
	ok = resumeBrokerProcesses(launch.Processes, processes)
	writeBrokerResponse(connection, ok)
	return true, ok
}

func writeBrokerResponse(writer io.Writer, ok bool) {
	_ = writeBrokerFrame(writer, brokerResponse{Version: fuseprotocol.Version, OK: ok, Error: ""})
}

func validateBrokerLaunch(launch brokerLaunch) error {
	if launch.Version != fuseprotocol.Version || !fuseprotocol.ValidIdentity(launch.RuntimeUID) || launch.Generation <= 0 {
		return fmt.Errorf("invalid broker identity")
	}
	if !validTokenPart(launch.SocketID, 22) || !validTokenPart(launch.Secret, 43) || len(launch.Processes) > 32768 {
		return fmt.Errorf("invalid broker token material")
	}
	seen := make(map[int]struct{}, len(launch.Processes))
	for _, process := range launch.Processes {
		if process.PID <= 0 || process.UID != requiredUID || process.StartTime == 0 || !isStopped(process.State) {
			return fmt.Errorf("invalid stopped process identity")
		}
		if _, duplicate := seen[process.PID]; duplicate {
			return fmt.Errorf("duplicate stopped process identity")
		}
		seen[process.PID] = struct{}{}
	}
	return nil
}

func parseOpaqueToken(token string) (socketID, secret string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" || !validTokenPart(parts[1], 22) || !validTokenPart(parts[2], 43) {
		return "", "", ErrInvalidToken
	}
	return parts[1], parts[2], nil
}

func validTokenPart(value string, encodedLength int) bool {
	if len(value) != encodedLength {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil
}

func abstractSocket(socketID string) string { return "\x00workspace-probe-" + socketID }

func decodeLimitedJSON(reader io.Reader, out any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, fuseprotocol.MaxJSONBytes+1))
	if err != nil || len(raw) > fuseprotocol.MaxJSONBytes {
		return fmt.Errorf("invalid broker message size")
	}
	return fuseprotocol.DecodeExact(raw, out)
}

func writeBrokerFrame(writer io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > fuseprotocol.MaxJSONBytes {
		return fmt.Errorf("invalid broker frame")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	if err := writeAll(writer, size[:]); err != nil {
		return err
	}
	return writeAll(writer, raw)
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

func readBrokerFrame(reader io.Reader, out any) error {
	var size [4]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > fuseprotocol.MaxJSONBytes {
		return fmt.Errorf("invalid broker frame size")
	}
	raw := make([]byte, int(length))
	if _, err := io.ReadFull(reader, raw); err != nil {
		return err
	}
	return fuseprotocol.DecodeExact(raw, out)
}

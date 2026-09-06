package workspaceprobe

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const (
	// PID1EnvironmentName and PID1EnvironmentValue form the image/runtime
	// contract that turns the otherwise public `self-check` invocation into the
	// Kubernetes sandbox's trusted PID 1 ledger and child reaper.
	PID1EnvironmentName       = fuseprotocol.ProbePID1EnvironmentName
	PID1EnvironmentValue      = fuseprotocol.ProbePID1EnvironmentValue
	RuntimeUIDEnvironmentName = "SANDBOX_POD_UID"
	pid1Socket                = "\x00workspace-probe-pid1-v1"
)

type pid1Broker struct{ processes processTable }

type pid1Request struct {
	Version    int               `json:"version"`
	Command    string            `json:"command"`
	RuntimeUID string            `json:"runtime_uid"`
	Generation int64             `json:"generation"`
	Processes  []processIdentity `json:"processes"`
	Token      string            `json:"token"`
}

type pid1Response struct {
	Version   int    `json:"version"`
	OK        bool   `json:"ok"`
	Token     string `json:"token"`
	ErrorCode string `json:"error_code"`
}

type pid1Ledger struct {
	mu         sync.Mutex
	runtimeUID string
	active     *pid1Cycle
}

type pid1Cycle struct {
	RuntimeUID string
	Generation int64
	Processes  []processIdentity
	Token      string
}

func (b *pid1Broker) protectedProcess() (*processIdentity, bool, error) {
	connection, err := net.DialTimeout("unix", pid1Socket, 150*time.Millisecond)
	if err != nil {
		return nil, false, nil
	}
	defer connection.Close()
	pid, uid, err := unixPeerIdentity(connection)
	if err != nil || pid != 1 || uid != requiredUID {
		return nil, false, fmt.Errorf("unexpected pid 1 broker peer")
	}
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	request := pid1Request{Version: fuseprotocol.Version, Command: "ping", Processes: []processIdentity{}}
	if err := writeBrokerFrame(connection, request); err != nil {
		return nil, false, err
	}
	var response pid1Response
	if err := readBrokerFrame(connection, &response); err != nil || response.Version != fuseprotocol.Version || !response.OK {
		return nil, false, fmt.Errorf("pid 1 broker ping failed")
	}
	identity, err := b.processes.identity(1)
	if err != nil || identity.PID != 1 || identity.UID != requiredUID {
		return nil, false, fmt.Errorf("pid 1 identity cannot be verified")
	}
	return &identity, true, nil
}

func (b *pid1Broker) start(state brokerState) (string, error) {
	connection, err := net.DialTimeout("unix", pid1Socket, time.Second)
	if err != nil {
		return "", fmt.Errorf("pid 1 broker disappeared")
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	request := pid1Request{
		Version: fuseprotocol.Version, Command: "register", RuntimeUID: state.RuntimeUID,
		Generation: state.Generation, Processes: cloneProcessSnapshot(state.Processes), Token: "",
	}
	if err := writeBrokerFrame(connection, request); err != nil {
		return "", err
	}
	var response pid1Response
	if err := readBrokerFrame(connection, &response); err != nil || response.Version != fuseprotocol.Version {
		return "", fmt.Errorf("pid 1 broker rejected quiesce registration")
	}
	if !response.OK || !validPID1Token(response.Token) {
		return "", fmt.Errorf("pid 1 broker rejected quiesce registration: %s", safePID1ErrorCode(response.ErrorCode))
	}
	return response.Token, nil
}

func safePID1ErrorCode(code string) string {
	switch code {
	case "invalid_request", "invalid_command", "invalid_ping", "invalid_registration", "unverified_process", "entropy_failure", "cycle_active":
		return code
	default:
		return "rejected"
	}
}

func (b *pid1Broker) resume(request resumeRequest) error {
	if !validPID1Token(request.Token) {
		return ErrInvalidToken
	}
	connection, err := net.DialTimeout("unix", pid1Socket, time.Second)
	if err != nil {
		return ErrInvalidToken
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	wire := pid1Request{
		Version: fuseprotocol.Version, Command: "resume", RuntimeUID: request.RuntimeUID,
		Generation: request.Generation, Processes: []processIdentity{}, Token: request.Token,
	}
	if err := writeBrokerFrame(connection, wire); err != nil {
		return ErrInvalidToken
	}
	var response pid1Response
	if err := readBrokerFrame(connection, &response); err != nil || response.Version != fuseprotocol.Version || !response.OK || response.Token != "" {
		return ErrInvalidToken
	}
	return nil
}

// MaybeRunPID1 enters the internal Kubernetes PID 1 mode only when both the
// fixed image environment and immutable process/identity constraints match.
// Other execs inherit the environment but retain the public four-command CLI.
func MaybeRunPID1() (bool, int) {
	if os.Getenv(PID1EnvironmentName) != PID1EnvironmentValue || os.Getpid() != 1 {
		return false, 0
	}
	if !validPID1Invocation(os.Getpid(), os.Geteuid(), os.Getegid(), os.Getenv(PID1EnvironmentName), os.Args) {
		return true, 1
	}
	if err := os.Chdir("/"); err != nil {
		return true, 1
	}
	if err := disableProcessDump(); err != nil {
		return true, 1
	}
	runtimeUID := os.Getenv(RuntimeUIDEnvironmentName)
	if !fuseprotocol.ValidIdentity(runtimeUID) {
		return true, 1
	}
	info, err := os.Stat(defaultWorkspace)
	if err != nil || !info.IsDir() {
		return true, 1
	}
	listener, err := net.Listen("unix", pid1Socket)
	if err != nil {
		return true, 1
	}
	defer listener.Close()
	startChildReaper()
	ledger := &pid1Ledger{runtimeUID: runtimeUID}
	for {
		connection, err := listener.Accept()
		if err != nil {
			return true, 1
		}
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		ledger.handle(connection, procFS{root: "/proc"})
		_ = connection.Close()
	}
}

func validPID1Invocation(pid, uid, gid int, environment string, argv []string) bool {
	return pid == 1 && uid == requiredUID && gid == requiredGID && environment == PID1EnvironmentValue &&
		len(argv) == 2 && argv[0] == fuseprotocol.ProbeBinary && argv[1] == "self-check"
}

func (l *pid1Ledger) handle(connection net.Conn, processes processTable) {
	var request pid1Request
	if err := readBrokerFrame(connection, &request); err != nil || request.Version != fuseprotocol.Version {
		writePID1Response(connection, false, "", "invalid_request")
		return
	}
	switch request.Command {
	case "ping":
		if request.RuntimeUID != "" || request.Generation != 0 || len(request.Processes) != 0 || request.Token != "" {
			writePID1Response(connection, false, "", "invalid_ping")
			return
		}
		writePID1Response(connection, true, "", "")
	case "register":
		l.register(connection, request, processes)
	case "resume":
		l.resume(connection, request, processes)
	default:
		writePID1Response(connection, false, "", "invalid_command")
	}
}

func (l *pid1Ledger) register(connection net.Conn, request pid1Request, processes processTable) {
	if request.Token != "" || request.RuntimeUID != l.runtimeUID || !fuseprotocol.ValidIdentity(request.RuntimeUID) || request.Generation <= 0 || len(request.Processes) > 32768 {
		writePID1Response(connection, false, "", "invalid_registration")
		return
	}
	seen := make(map[int]struct{}, len(request.Processes))
	for _, expected := range request.Processes {
		current, err := processes.identity(expected.PID)
		_, duplicate := seen[expected.PID]
		if err != nil || duplicate || expected.PID == 1 || expected.UID != requiredUID || !isStopped(expected.State) || current.UID != expected.UID || current.StartTime != expected.StartTime || !isStopped(current.State) {
			writePID1Response(connection, false, "", "unverified_process")
			return
		}
		seen[expected.PID] = struct{}{}
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		writePID1Response(connection, false, "", "entropy_failure")
		return
	}
	token := "p1." + base64.RawURLEncoding.EncodeToString(tokenBytes)
	wipe(tokenBytes)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active != nil {
		writePID1Response(connection, false, "", "cycle_active")
		return
	}
	l.active = &pid1Cycle{RuntimeUID: request.RuntimeUID, Generation: request.Generation, Processes: cloneProcessSnapshot(request.Processes), Token: token}
	writePID1Response(connection, true, token, "")
}

func (l *pid1Ledger) resume(connection net.Conn, request pid1Request, processes processTable) {
	if len(request.Processes) != 0 || request.Token == "" {
		writePID1Response(connection, false, "", "invalid_resume")
		return
	}
	l.mu.Lock()
	cycle := l.active
	if cycle == nil || cycle.RuntimeUID != request.RuntimeUID || cycle.Generation != request.Generation ||
		subtle.ConstantTimeCompare([]byte(cycle.Token), []byte(request.Token)) != 1 {
		l.mu.Unlock()
		writePID1Response(connection, false, "", "invalid_token")
		return
	}
	l.active = nil
	l.mu.Unlock()
	if !resumeBrokerProcesses(cycle.Processes, processes) {
		writePID1Response(connection, false, "", "resume_failed")
		return
	}
	writePID1Response(connection, true, "", "")
}

func resumeBrokerProcesses(expectedProcesses []processIdentity, processes processTable) bool {
	for _, expected := range expectedProcesses {
		current, err := processes.identity(expected.PID)
		if err != nil || current.UID != expected.UID || current.StartTime != expected.StartTime || !isStopped(current.State) {
			return false
		}
	}
	resumed := make([]processIdentity, 0, len(expectedProcesses))
	for _, expected := range expectedProcesses {
		if err := processes.signal(expected.PID, syscall.SIGCONT); err != nil {
			for _, process := range resumed {
				if current, identityErr := processes.identity(process.PID); identityErr == nil && current.UID == process.UID && current.StartTime == process.StartTime {
					_ = processes.signal(process.PID, syscall.SIGSTOP)
				}
			}
			return false
		}
		resumed = append(resumed, expected)
	}
	return true
}

func writePID1Response(writer net.Conn, ok bool, token, code string) {
	_ = writeBrokerFrame(writer, pid1Response{Version: fuseprotocol.Version, OK: ok, Token: token, ErrorCode: code})
}

func validPID1Token(token string) bool {
	if len(token) != 46 || len(token) < 3 || token[:3] != "p1." {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token[3:])
	return err == nil
}

func startChildReaper() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGCHLD)
	go func() {
		for range signals {
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if err != nil || pid <= 0 {
					break
				}
			}
		}
	}()
}

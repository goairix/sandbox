package workspaceprobe

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

func TestLocalBrokerChildNeverStartsInWorkspace(t *testing.T) {
	command := localBrokerCommand("/usr/local/bin/workspace-probe", brokerState{RuntimeUID: "runtime-a", Generation: 7})
	assert.Equal(t, "/", command.Dir)
	assert.Nil(t, command.Stdin)
	assert.Nil(t, command.Stdout)
	assert.Nil(t, command.Stderr)
}

func TestBrokerRejectsWrongSecretWithoutConsumingOrResuming(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{10: {PID: 10, UID: 1000, StartTime: 42, State: 'T'}}}
	launch := brokerLaunch{Version: 1, RuntimeUID: "runtime-a", Generation: 7, Secret: "secret", Processes: []processIdentity{{PID: 10, UID: 1000, StartTime: 42, State: 'T'}}}
	server, client := net.Pipe()
	done := make(chan struct{})
	var consumed, ok bool
	go func() {
		consumed, ok = handleBrokerConnection(server, launch, processes)
		_ = server.Close()
		close(done)
	}()
	require.NoError(t, writeBrokerFrame(client, brokerResumeWire{Version: 1, RuntimeUID: "runtime-a", Generation: 7, Secret: "wrong"}))
	var response brokerResponse
	require.NoError(t, readBrokerFrame(client, &response))
	_ = client.Close()
	<-done
	assert.False(t, consumed)
	assert.False(t, ok)
	assert.Empty(t, processes.resumed)
}

func TestBrokerAtomicallyConsumesValidTokenAndResumesExactPIDs(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{10: {PID: 10, UID: 1000, StartTime: 42, State: 'T'}}}
	launch := brokerLaunch{Version: 1, RuntimeUID: "runtime-a", Generation: 7, Secret: "secret", Processes: []processIdentity{{PID: 10, UID: 1000, StartTime: 42, State: 'T'}}}
	server, client := net.Pipe()
	done := make(chan struct{})
	var consumed, ok bool
	go func() {
		consumed, ok = handleBrokerConnection(server, launch, processes)
		_ = server.Close()
		close(done)
	}()
	require.NoError(t, writeBrokerFrame(client, brokerResumeWire{Version: 1, RuntimeUID: "runtime-a", Generation: 7, Secret: "secret"}))
	var response brokerResponse
	require.NoError(t, readBrokerFrame(client, &response))
	_ = client.Close()
	<-done
	assert.True(t, consumed)
	assert.True(t, ok)
	assert.True(t, response.OK)
	assert.Equal(t, []int{10}, processes.resumed)
}

func TestBrokerConsumesValidTokenButRefusesPIDReuse(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{10: {PID: 10, UID: 1000, StartTime: 99, State: 'T'}}}
	launch := brokerLaunch{Version: 1, RuntimeUID: "runtime-a", Generation: 7, Secret: "secret", Processes: []processIdentity{{PID: 10, UID: 1000, StartTime: 42, State: 'T'}}}
	server, client := net.Pipe()
	done := make(chan struct{})
	var consumed, ok bool
	go func() {
		consumed, ok = handleBrokerConnection(server, launch, processes)
		_ = server.Close()
		close(done)
	}()
	require.NoError(t, writeBrokerFrame(client, brokerResumeWire{Version: 1, RuntimeUID: "runtime-a", Generation: 7, Secret: "secret"}))
	var response brokerResponse
	require.NoError(t, readBrokerFrame(client, &response))
	_ = client.Close()
	<-done
	assert.True(t, consumed)
	assert.False(t, ok)
	assert.Empty(t, processes.resumed)
}

func TestReaperContractRequiresSIGCHLDHandling(t *testing.T) {
	const sigchld = uint64(1) << (uint64(syscall.SIGCHLD) - 1)
	require.NoError(t, validateReaperSignals(sigchld, 0, 0))
	require.Error(t, validateReaperSignals(0, sigchld, 0), "a caught signal does not prove that pid 1 calls wait")
	require.Error(t, validateReaperSignals(0, 0, sigchld), "blocking SIGCHLD does not prove that pid 1 waits for children")
	require.Error(t, validateReaperSignals(0, 0, 0))
}

func TestDockerReaperHandshakeReplacesSignalDispositionFallback(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{1: {PID: 1, UID: 0, StartTime: 11, State: 'S'}}}
	client := &dockerReaperClient{
		processes: processes,
		peer:      func(net.Conn) (int, int, error) { return 1, 0, nil },
	}
	client.dial = func() (net.Conn, error) {
		server, caller := net.Pipe()
		go func() {
			defer server.Close()
			var request fuseprotocol.DockerReaperRequest
			if readBrokerFrame(server, &request) != nil {
				return
			}
			accepted := request.Version == 1 && request.UID == 1000 &&
				((request.Command == "ping" && request.PID == 0) || (request.Command == "register" && request.PID == 20 && request.StartTime == 99))
			_ = writeBrokerFrame(server, fuseprotocol.DockerReaperResponse{Version: 1, Accepted: accepted, ErrorCode: ""})
		}()
		return caller, nil
	}
	identity, available, err := client.protectedProcess()
	require.NoError(t, err)
	assert.True(t, available)
	assert.Equal(t, 1, identity.PID)
	require.NoError(t, client.register(processIdentity{PID: 20, UID: 1000, StartTime: 99, State: 'S'}))
}

func TestRootPID1WithoutDockerReaperHandshakeFailsClosed(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{1: {PID: 1, UID: 0, StartTime: 11, State: 'S'}}}
	broker := newHybridBroker(processes, bytes.NewReader(make([]byte, 64)))
	broker.dockerReaper.dial = func() (net.Conn, error) { return nil, syscall.ECONNREFUSED }

	_, err := broker.protectedProcess()
	require.ErrorContains(t, err, "trusted Docker pid 1 reaper handshake is unavailable")
}

func TestUnreadablePID1IdentityWithoutDockerReaperHandshakeFailsClosed(t *testing.T) {
	processes := &fakeProcesses{
		states:         map[int]processIdentity{},
		identityErrors: map[int]error{1: syscall.EACCES},
	}
	broker := newHybridBroker(processes, bytes.NewReader(make([]byte, 64)))
	broker.dockerReaper.dial = func() (net.Conn, error) { return nil, syscall.ECONNREFUSED }

	_, err := broker.protectedProcess()
	require.ErrorContains(t, err, "pid 1 identity cannot be verified")
}

func TestPID1InvocationRequiresFixedImageContract(t *testing.T) {
	assert.True(t, validPID1Invocation(1, 1000, 1000, PID1EnvironmentValue, []string{"/usr/local/bin/workspace-probe", "self-check"}))
	for _, valid := range []bool{
		validPID1Invocation(2, 1000, 1000, PID1EnvironmentValue, []string{"/usr/local/bin/workspace-probe", "self-check"}),
		validPID1Invocation(1, 0, 1000, PID1EnvironmentValue, []string{"/usr/local/bin/workspace-probe", "self-check"}),
		validPID1Invocation(1, 1000, 1000, "user", []string{"/usr/local/bin/workspace-probe", "self-check"}),
		validPID1Invocation(1, 1000, 1000, PID1EnvironmentValue, []string{"/usr/local/bin/workspace-probe", "quiesce"}),
	} {
		assert.False(t, valid)
	}
}

func TestSafePID1ErrorCodeAllowsOnlyFixedDiagnostics(t *testing.T) {
	for _, code := range []string{"invalid_request", "invalid_command", "invalid_ping", "invalid_registration", "unverified_process", "entropy_failure", "cycle_active"} {
		assert.Equal(t, code, safePID1ErrorCode(code))
	}
	assert.Equal(t, "rejected", safePID1ErrorCode("secret-derived-detail"))
	assert.Equal(t, "rejected", safePID1ErrorCode(""))
}

func TestBrokerProcessSnapshotIsNeverEncodedAsNull(t *testing.T) {
	processes := cloneProcessSnapshot(nil)
	require.NotNil(t, processes)
	raw, err := json.Marshal(pid1Request{
		Version: 1, Command: "register", RuntimeUID: "runtime-a", Generation: 7,
		Processes: processes, Token: "",
	})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"processes":[]`)
	var decoded pid1Request
	require.NoError(t, fuseprotocol.DecodeExact(raw, &decoded))
}

func TestBrokerFrameRejectsDuplicateFields(t *testing.T) {
	raw := []byte(`{"version":1,"version":1,"runtime_uid":"runtime-a","generation":7,"secret":"secret"}`)
	var framed bytes.Buffer
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	framed.Write(size[:])
	framed.Write(raw)
	var request brokerResumeWire
	require.Error(t, readBrokerFrame(&framed, &request))
}

func TestPID1LedgerRejectsCrossGenerationAndReplay(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{10: {PID: 10, UID: 1000, StartTime: 42, State: 'T'}}}
	ledger := &pid1Ledger{runtimeUID: "runtime-a"}
	register := pid1Request{
		Version: 1, Command: "register", RuntimeUID: "runtime-a", Generation: 7,
		Processes: []processIdentity{{PID: 10, UID: 1000, StartTime: 42, State: 'T'}}, Token: "",
	}
	response := callPID1Ledger(t, ledger, processes, register)
	require.True(t, response.OK)
	require.True(t, validPID1Token(response.Token))

	cross := callPID1Ledger(t, ledger, processes, pid1Request{
		Version: 1, Command: "resume", RuntimeUID: "runtime-a", Generation: 8,
		Processes: []processIdentity{}, Token: response.Token,
	})
	assert.False(t, cross.OK)
	assert.Empty(t, processes.resumed)
	crossRuntime := callPID1Ledger(t, ledger, processes, pid1Request{
		Version: 1, Command: "resume", RuntimeUID: "runtime-b", Generation: 7,
		Processes: []processIdentity{}, Token: response.Token,
	})
	assert.False(t, crossRuntime.OK)
	assert.Empty(t, processes.resumed)

	valid := callPID1Ledger(t, ledger, processes, pid1Request{
		Version: 1, Command: "resume", RuntimeUID: "runtime-a", Generation: 7,
		Processes: []processIdentity{}, Token: response.Token,
	})
	assert.True(t, valid.OK)
	assert.Equal(t, []int{10}, processes.resumed)

	replay := callPID1Ledger(t, ledger, processes, pid1Request{
		Version: 1, Command: "resume", RuntimeUID: "runtime-a", Generation: 7,
		Processes: []processIdentity{}, Token: response.Token,
	})
	assert.False(t, replay.OK)
}

func TestPID1LedgerRejectsDuplicatePIDRegistration(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{10: {PID: 10, UID: 1000, StartTime: 42, State: 'T'}}}
	ledger := &pid1Ledger{runtimeUID: "runtime-a"}
	identity := processIdentity{PID: 10, UID: 1000, StartTime: 42, State: 'T'}
	response := callPID1Ledger(t, ledger, processes, pid1Request{
		Version: 1, Command: "register", RuntimeUID: "runtime-a", Generation: 7,
		Processes: []processIdentity{identity, identity}, Token: "",
	})
	assert.False(t, response.OK)
	assert.Nil(t, ledger.active)
}

func TestPID1LedgerRejectsRegistrationForDifferentRuntime(t *testing.T) {
	processes := &fakeProcesses{states: map[int]processIdentity{}}
	ledger := &pid1Ledger{runtimeUID: "runtime-a"}
	response := callPID1Ledger(t, ledger, processes, pid1Request{
		Version: 1, Command: "register", RuntimeUID: "runtime-b", Generation: 7,
		Processes: []processIdentity{}, Token: "",
	})
	assert.False(t, response.OK)
	assert.Nil(t, ledger.active)
}

func callPID1Ledger(t *testing.T, ledger *pid1Ledger, processes processTable, request pid1Request) pid1Response {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		ledger.handle(server, processes)
		_ = server.Close()
		close(done)
	}()
	require.NoError(t, writeBrokerFrame(client, request))
	var response pid1Response
	require.NoError(t, readBrokerFrame(client, &response))
	_ = client.Close()
	<-done
	return response
}

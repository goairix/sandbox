package workspaceprobe

import (
	"bytes"
	"encoding/binary"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

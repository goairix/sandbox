//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
)

// Only this test ELF has a fixture clock/root signer. Production launcher has no
// test env/config mode or key supplied by the command request.
type nativeFixtureClock struct{}

func (nativeFixtureClock) Observe(ctx context.Context) (controlprotocol.ClockObservation, error) {
	return controlprotocol.ClockObservation{UTC: time.Now().UTC(), Uncertainty: time.Millisecond}, ctx.Err()
}

var nativeOnce sync.Once
var nativeKernel *launcher.KernelBoundary

func requireNative(t *testing.T) *launcher.KernelBoundary {
	t.Helper()
	if os.Getenv("SANDBOX_TASK3_NATIVE") != "1" {
		t.Skip("requires controller-owned actual PID1 fixture")
	}
	if os.Getpid() != 1 {
		t.Fatal("native test must be PID1")
	}
	nativeOnce.Do(func() {
		var e error
		nativeKernel, e = launcher.BootstrapPID1()
		if e != nil {
			t.Fatal(e)
		}
	})
	return nativeKernel
}

type fixtureAttestation struct {
	Version        int    `json:"version"`
	RuntimeID      string `json:"runtime_id"`
	RuntimeUID     string `json:"runtime_uid"`
	ImageID        string `json:"image_id"`
	Architecture   string `json:"architecture"`
	ContractDigest string `json:"contract_digest"`
	ContractJSON   string `json:"contract_json"`
}

func nativeAttestation(t *testing.T) fixtureAttestation {
	t.Helper()
	data, e := os.ReadFile(os.Getenv("SANDBOX_TASK3_ATTESTATION"))
	if e != nil {
		t.Fatal(e)
	}
	var a fixtureAttestation
	if e = decodeMonitorJSON(data, &a); e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256([]byte(a.ContractJSON))
	if a.Version != 1 || len(a.RuntimeID) != 64 || a.RuntimeUID != a.RuntimeID || a.ContractDigest != hex.EncodeToString(h[:]) {
		t.Fatal("invalid inspect-bound fixture attestation")
	}
	return a
}

type nativeFixture struct {
	s              *Supervisor
	issuer         ed25519.PrivateKey
	issuerWire     []byte
	issuerIdentity controlprotocol.CommandIssuerIdentity
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func newNativeFixture(t *testing.T) nativeFixture {
	t.Helper()
	kernel := requireNative(t)
	a := nativeAttestation(t)
	pub, root, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	issuerPub, issuer, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	binding := controlprotocol.TrustBinding{Namespace: "/sandbox/native/v1/", AuthorityID: "fixture", Target: "target", RestoreEpoch: "restore"}
	verifier, e := controlprotocol.NewManagementVerifier(binding, []ed25519.PublicKey{pub})
	if e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll("/journal/protected", 0700); e != nil {
		t.Fatal(e)
	}
	s, e := NewSupervisor(SupervisorOptions{Kernel: kernel, UID: 1000, GID: 1000, ContractDigest: a.ContractDigest, Verifier: verifier, Clock: nativeFixtureClock{}, JournalDirectory: "/journal/protected/" + uuid.NewString(), Executable: os.Getenv("SANDBOX_TASK3_MONITOR")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	now := time.Now().UTC()
	nb, na := now.Add(-time.Minute), now.Add(10*time.Minute)
	iw, e := controlprotocol.SignCommandIssuerCertificate(root, controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerPub, NotBefore: nb, NotAfter: na})
	if e != nil {
		t.Fatal(e)
	}
	birth := s.Birth()
	identity := controlprotocol.RuntimeIdentityContext{SandboxID: "native", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: controlprotocol.RuntimeReference{ID: a.RuntimeID, UID: a.RuntimeUID, BootID: birth.BootID}}
	rw, e := controlprotocol.SignRuntimeIdentityCertificate(root, controlprotocol.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: 1, Runtime: identity.Runtime})
	if e != nil {
		t.Fatal(e)
	}
	aw, e := controlprotocol.SignTargetActivation(root, controlprotocol.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: identity, DataGateEpoch: 1, UID: 1000, GID: 1000, ContractDigest: a.ContractDigest, RuntimeCertificateDigest: hashBytes(rw), IssuerCertificateDigest: hashBytes(iw), NotBefore: nb, NotAfter: na})
	if e != nil {
		t.Fatal(e)
	}
	activation, e := verifier.VerifyTargetActivation(aw, rw, iw, birth, now)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Activate(context.Background(), activation); e != nil {
		t.Fatal(e)
	}
	issuerIdentity, e := verifier.VerifyCommandIssuerCertificate(iw, now)
	if e != nil {
		t.Fatal(e)
	}
	return nativeFixture{s: s, issuer: issuer, issuerWire: iw, issuerIdentity: issuerIdentity}
}
func (f nativeFixture) start(t *testing.T, request controlprotocol.ExecutionRequest, window time.Duration) (*Execution, *authenticatedStart) {
	t.Helper()
	d, e := controlprotocol.NewExecutionDescriptor(request)
	if e != nil {
		t.Fatal(e)
	}
	a := f.s.activation
	i, b := a.Identity(), a.Binding()
	now := time.Now().UTC()
	c := controlprotocol.ExecStartContext{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, IssuerCertificateID: f.issuerIdentity.CertificateID(), IssuerCertificateDigest: f.issuerIdentity.Digest(), CommandID: uuid.NewString(), OperationID: uuid.NewString(), RequestID: "request", OperationDigest: strings.Repeat("b", 64), SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, DataGateEpoch: 1, ControlRevision: 1, AdmissionRevision: 1, LeaseID: 1, Runtime: i.Runtime, ExpiresAt: now.Add(time.Minute)}
	ticket, e := controlprotocol.SignExecStartTicket(f.issuer, f.issuerIdentity, controlprotocol.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: d.Digest(), NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(window)})
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := f.s.options.Verifier.VerifyExecStartTicket(ticket, f.issuerWire, c, d, now)
	if e != nil {
		t.Fatal(e)
	}
	start := &authenticatedStart{supervisor: f.s, descriptor: d, evidence: evidence}
	execution, e := f.s.accept(context.Background(), start)
	if e != nil {
		t.Fatal(e)
	}
	return execution, start
}
func TestTask3NativeProductionMonitor(t *testing.T) {
	f := newNativeFixture(t)
	input := []byte{0, 255, 128, 10, 1}
	request := controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "binary", "a b"}, Env: map[string]string{"GOMAXPROCS": "2", "PATH": "/signed-only", "VALUE": "literal $()"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 10, Stdin: input}
	e, start := f.start(t, request, 10*time.Second)
	if len(e.acceptedReceipt) == 0 {
		t.Fatal("accept ACK missing before completion")
	}
	if _, err := f.s.accept(context.Background(), start); err == nil {
		t.Fatal("duplicate spawned again")
	}
	var out, stderr bytes.Buffer
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case frame, ok := <-e.output:
			if !ok {
				goto completed
			}
			if frame.kind == monitorStdout {
				out.Write(frame.data)
			} else {
				stderr.Write(frame.data)
			}
		case <-deadline.C:
			t.Fatal("bounded output not completed")
		}
	}
completed:
	<-e.ownerDone
	e.mu.Lock()
	state, record := e.state, e.record
	e.mu.Unlock()
	if state != "local_terminal" || record.RootPID <= 1 || record.RootWaitStatus != 0 || !record.DrainConfirmed {
		t.Fatalf("terminal state=%s record=%+v", state, record)
	}
	var got struct {
		Stdin    []byte `json:"stdin"`
		Cwd      string `json:"cwd"`
		UID, GID int
		Groups   []int
		Env      []string
		Argv     []string
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatalf("output %q %v", out.Bytes(), err)
	}
	if !bytes.Equal(got.Stdin, input) || got.Cwd != "/tmp" || got.UID != 1000 || got.GID != 1000 || len(got.Groups) != 0 || !bytes.Contains(stderr.Bytes(), []byte("STDERR_MARKER")) {
		t.Fatalf("actual user %+v stderr=%q", got, stderr.Bytes())
	}
	t.Logf("production monitor actual root=%d raw=%d drain=%t output=%q stderr=%q", record.RootPID, record.RootWaitStatus, record.DrainConfirmed, out.Bytes(), stderr.Bytes())
	fmt.Println("TASK3_PRODUCTION_MONITOR_PASS")
}

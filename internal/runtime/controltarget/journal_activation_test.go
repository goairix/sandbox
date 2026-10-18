//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type liveClock struct {
	now   time.Time
	calls int
	err   error
}

func (c *liveClock) Observe(context.Context) (controlprotocol.ClockObservation, error) {
	c.calls++
	return controlprotocol.ClockObservation{UTC: c.now}, c.err
}

type liveFixture struct {
	o          JournalOptions
	clock      *liveClock
	activation controlprotocol.TargetActivationEvidence
	e          controlprotocol.ExecStartEvidence
	rootKey    ed25519.PrivateKey
	issuerKey  ed25519.PrivateKey
	runtimeKey ed25519.PrivateKey
	issuerWire []byte
	descriptor controlprotocol.ExecutionDescriptor
	claims     controlprotocol.ExecStartTicketClaims
}

func liveSetup(t *testing.T) liveFixture {
	t.Helper()
	now := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	root, rk, _ := ed25519.GenerateKey(rand.Reader)
	rp, runtimeKey, _ := ed25519.GenerateKey(rand.Reader)
	ip, ik, _ := ed25519.GenerateKey(rand.Reader)
	o := journalOptionsFixture(t)
	o.Identity.Runtime.BootID = "6a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	i := o.Identity
	b := controlprotocol.TrustBinding{Namespace: i.Namespace, AuthorityID: i.AuthorityID, Target: i.Target, RestoreEpoch: i.RestoreEpoch}
	v, err := controlprotocol.NewManagementVerifier(b, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	before, after := now.Add(-time.Minute), now.Add(time.Hour)
	iw, err := controlprotocol.SignCommandIssuerCertificate(rk, controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: "11111111-1111-4111-8111-111111111111", Role: "command_issuer", Namespace: i.Namespace, AuthorityID: i.AuthorityID, Target: i.Target, RestoreEpoch: i.RestoreEpoch, PublicKey: ip, NotBefore: before, NotAfter: after})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := v.VerifyCommandIssuerCertificate(iw, now)
	if err != nil {
		t.Fatal(err)
	}
	rw, err := controlprotocol.SignRuntimeIdentityCertificate(rk, controlprotocol.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: "51111111-1111-4111-8111-111111111111", Role: "runtime_receipt", Namespace: i.Namespace, AuthorityID: i.AuthorityID, Target: i.Target, RestoreEpoch: i.RestoreEpoch, PublicKey: rp, NotBefore: before, NotAfter: after, SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, Runtime: i.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	digest := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	birth := controlprotocol.BirthContext{BootID: i.Runtime.BootID, RuntimePublicKey: rp, UID: 1000, GID: 1001, ContractDigest: strings.Repeat("e", 64)}
	ac := controlprotocol.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: "71111111-1111-4111-8111-111111111111", Binding: b, Identity: controlprotocol.RuntimeIdentityContext{SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, Runtime: i.Runtime}, DataGateEpoch: o.DataGateEpoch, UID: birth.UID, GID: birth.GID, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: digest(rw), IssuerCertificateDigest: digest(iw), NotBefore: before, NotAfter: now.Add(30 * time.Minute)}
	aw, err := controlprotocol.SignTargetActivation(rk, ac)
	if err != nil {
		t.Fatal(err)
	}
	a, err := v.VerifyTargetActivation(aw, rw, iw, birth, now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := controlprotocol.NewExecutionDescriptor(controlprotocol.ExecutionRequest{Argv: []string{"/bin/true"}, Env: map[string]string{"PATH": "/bin"}, UID: birth.UID, GID: birth.GID, WorkDir: "/", TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	c := journalRecordFixture().Context
	c.Runtime = i.Runtime
	c.IssuerCertificateID = issuer.CertificateID()
	c.IssuerCertificateDigest = issuer.Digest()
	c.ExpiresAt = now.Add(10 * time.Minute)
	claims := controlprotocol.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: d.Digest(), NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)}
	w, err := controlprotocol.SignExecStartTicket(ik, issuer, claims)
	if err != nil {
		t.Fatal(err)
	}
	e, err := v.VerifyExecStartTicket(w, iw, c, d, now)
	if err != nil {
		t.Fatal(err)
	}
	clock := &liveClock{now: now}
	o.Birth = &birth
	o.Verifier = v
	o.Clock = clock
	return liveFixture{o: o, clock: clock, activation: a, e: e, rootKey: rk, issuerKey: ik, runtimeKey: runtimeKey, issuerWire: iw, descriptor: d, claims: claims}
}
func (f liveFixture) create(t *testing.T) *Journal {
	t.Helper()
	j, err := CreateClosedJournal(context.Background(), f.o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}
func (f liveFixture) renew(t *testing.T) controlprotocol.ExecRenewEvidence {
	t.Helper()
	c := controlprotocol.ExecRenewTicketClaims(f.claims)
	c.Purpose = "operation_exec_renew"
	c.NotBefore = f.clock.now.Add(-2 * time.Second)
	c.NotAfter = f.clock.now.Add(25 * time.Second)
	w, err := controlprotocol.SignExecRenewTicket(f.issuerKey, c)
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.o.Verifier.VerifyExecRenewTicket(w, f.issuerWire, c.Context, c.DescriptorDigest, f.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestLiveJournalPartialConfigBeforeIO(t *testing.T) {
	f := liveSetup(t)
	for _, which := range []string{"birth", "verifier", "clock", "typednil"} {
		t.Run(which, func(t *testing.T) {
			o := f.o
			o.Directory = filepath.Join(protectedParent(t), "absent")
			switch which {
			case "birth":
				o.Birth = nil
			case "verifier":
				o.Verifier = nil
			case "clock":
				o.Clock = nil
			case "typednil":
				var clock *liveClock
				o.Clock = clock
			}
			if j, err := CreateClosedJournal(context.Background(), o); err == nil {
				j.Close()
				t.Fatal("partial config")
			}
			if _, err := os.Stat(o.Directory); !os.IsNotExist(err) {
				t.Fatal("invalid config mutated filesystem")
			}
		})
	}
}

func TestLiveJournalActivationOnly(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if f.clock.calls != 0 {
		t.Fatal("construction clock work")
	}
	// Constructor copied the public key and birth fields before any use.
	f.o.Birth.RuntimePublicKey[0] ^= 1
	f.o.Birth.UID++
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	if j.Status().Gate.GateState != "open" {
		t.Fatal("activation did not open live gate")
	}
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	if _, err := j.CloseGate(ctx, f.o.DataGateEpoch); err != nil {
		t.Fatal(err)
	}
	if err := j.InstallActivation(ctx, f.activation); err == nil {
		t.Fatal("close reopened activation")
	}
	j.Close()
	f.o.Birth = nil
	f.o.Verifier = nil
	f.o.Clock = nil
	// Historical expired wires can remain opaque diagnostics without time RPC.
	f.clock.now = f.clock.now.Add(24 * time.Hour)
	before := f.clock.calls
	cold, err := OpenClosedJournal(ctx, f.o)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	if f.clock.calls != before || cold.Status().Gate.GateState != "closed" {
		t.Fatal("cold recovery used clock or opened")
	}
	if err := cold.InstallActivation(ctx, f.activation); err == nil {
		t.Fatal("passive cold activated")
	}
}

func TestLiveJournalActivationProtectionAndMissing(t *testing.T) {
	for _, kind := range []string{"missing", "symlink", "hardlink", "mode", "digest", "unknown-field", "over-limit"} {
		t.Run(kind, func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			if err := j.InstallActivation(context.Background(), f.activation); err != nil {
				t.Fatal(err)
			}
			j.Close()
			p := filepath.Join(f.o.Directory, "activation.json")
			switch kind {
			case "missing":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(p, p+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p+".saved", p); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(p, filepath.Join(filepath.Dir(f.o.Directory), "link")); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(p, 0640); err != nil {
					t.Fatal(err)
				}
			case "digest":
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, p, []byte(strings.Replace(string(b), f.activation.Digest(), strings.Repeat("f", 64), 1)))
			case "unknown-field":
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, p, append([]byte(`{"unexpected":0,`), b[1:]...))
			case "over-limit":
				mustWrite(t, p, make([]byte, 16385))
			}
			if cold, err := OpenClosedJournal(context.Background(), f.o); err == nil {
				cold.Close()
				t.Fatal("unsafe activation adopted")
			}
		})
	}
}

func TestLiveJournalActivationFaults(t *testing.T) {
	for _, file := range []string{"activation", "gate"} {
		for _, op := range []string{"file-sync", "rename", "dir-sync"} {
			for _, after := range []bool{false, true} {
				t.Run(file+"/"+op+"/"+map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
					f := liveSetup(t)
					j := f.create(t)
					hit := false
					seen := 0
					j.files.hook = func(operation, name string, isAfter bool) error {
						if operation == op && after == isAfter {
							seen++
							wanted := 1
							if file == "gate" {
								wanted = 2
							}
							if seen == wanted {
								hit = true
								return fmt.Errorf("injected %s boundary", file)
							}
						}
						return nil
					}
					err := j.InstallActivation(context.Background(), f.activation)
					if !hit || err == nil || !j.Status().Poisoned || j.Status().Gate.GateState != "closed" {
						t.Fatal("activation uncertainty", hit, err, j.Status())
					}
					j.files.hook = nil
					j.Close()
					cold, err := OpenClosedJournal(context.Background(), f.o)
					if err == nil {
						defer cold.Close()
						if cold.Status().Gate.GateState != "closed" {
							t.Fatal("cold activated")
						}
						if err = cold.InstallActivation(context.Background(), f.activation); err == nil {
							t.Fatal("cold install")
						}
					}
				})
			}
		}
	}
}

func TestLiveJournalActivationBundleBounds(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	if err := j.InstallActivation(context.Background(), f.activation); err != nil {
		t.Fatal(err)
	}
	wire, err := os.ReadFile(filepath.Join(f.o.Directory, "activation.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append(wire, []byte(`{}`)...), bytes.Replace(wire, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(wire, []byte(`"wire":`), []byte(`"wire":null,"duplicate":`), 1)} {
		if _, err := decodeJournalActivation(bad); err == nil {
			t.Fatal("malformed bundle")
		}
	}
	b, err := decodeJournalActivation(wire)
	if err != nil {
		t.Fatal(err)
	}
	// An encoded bundle above the ordinary record bound still uses its own limit.
	b.RuntimeCertificate = bytes.Repeat([]byte("x"), 5500)
	large, err := encodeJournalWireLimit(b, maxJournalActivationBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(large) <= 8192 {
		t.Fatal("fixture not above record bound")
	}
	if _, err := decodeJournalActivation(large); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeJournalActivation(append(large, bytes.Repeat([]byte(" "), 16384-len(large))...)); err != nil {
		t.Fatal("16384 bounded activation", err)
	}
	if _, err := decodeJournalActivation(append(large, bytes.Repeat([]byte(" "), 16385-len(large))...)); err == nil {
		t.Fatal("16385 activation accepted")
	}
	b.RuntimeCertificate = bytes.Repeat([]byte("x"), 8193)
	if err = b.validate(); err == nil {
		t.Fatal("individual certificate limit")
	}
}

// This optional test interface keeps the existing package compilable before
// the leaf method exists. Absence is a failing behavior assertion, not a test
// skip or a substitute implementation of journal renewal.
type journalActivationRenewer interface {
	RenewActivation(context.Context, string, controlprotocol.TargetActivationEvidence) error
}

func requireJournalActivationRenewer(t *testing.T, j *Journal) journalActivationRenewer {
	t.Helper()
	renewer, ok := any(j).(journalActivationRenewer)
	if !ok {
		t.Fatal("live Journal must support origin-bound RenewActivation")
	}
	return renewer
}

// Each candidate is authenticated by the existing root verifier against its
// explicit independent birth. Negative candidates can be genuinely signed for
// another gate/birth; the receiving journal must still reject that rebinding.
func (f liveFixture) nextActivation(t *testing.T, mutate func(*controlprotocol.TargetActivationClaims, *controlprotocol.BirthContext)) controlprotocol.TargetActivationEvidence {
	t.Helper()
	a := f.activation
	birth := a.Birth()
	runtimeDigest := sha256.Sum256(a.RuntimeCertificate())
	issuerDigest := sha256.Sum256(a.IssuerCertificate())
	claims := controlprotocol.TargetActivationClaims{
		Version: 1, Role: "target_activation", ActivationID: "81111111-1111-4111-8111-111111111111",
		Binding: a.Binding(), Identity: a.Identity(), DataGateEpoch: a.DataGateEpoch(),
		UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed, ContractDigest: birth.ContractDigest,

		RuntimeCertificateDigest: hex.EncodeToString(runtimeDigest[:]), IssuerCertificateDigest: hex.EncodeToString(issuerDigest[:]),

		NotBefore: a.NotBefore().Add(10 * time.Second), NotAfter: a.NotAfter().Add(5 * time.Minute),
	}
	if mutate != nil {
		mutate(&claims, &birth)
	}
	wire, err := controlprotocol.SignTargetActivation(f.rootKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.o.Verifier.VerifyTargetActivation(wire, a.RuntimeCertificate(), a.IssuerCertificate(), birth, f.clock.now)
	if err != nil {
		t.Fatal("candidate must independently authenticate before journal admission", err)
	}
	return next
}

func TestLiveJournalRenewActivationRetainsOriginalCommands(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	accepted, err := j.AcceptExecution(ctx, f.e)
	if err != nil {
		t.Fatal(err)
	}
	original, err := accepted.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	next := f.nextActivation(t, nil)
	renewer := requireJournalActivationRenewer(t, j)
	if err := renewer.RenewActivation(ctx, f.activation.Digest(), next); err != nil {
		t.Fatal("renew open journal", err)
	}
	wire, err := os.ReadFile(filepath.Join(f.o.Directory, "activation.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := decodeJournalActivation(wire)
	if err != nil || bundle.Digest != next.Digest() || !bytes.Equal(bundle.RuntimeCertificate, next.RuntimeCertificate()) {
		t.Fatal("renewal did not persist the selected activation", err)
	}

	// Lost acknowledgement retries identify the original predecessor/candidate
	// pair. An idempotent retry must not replace any file a second time.
	writes := 0
	j.files.hook = func(operation, _ string, _ bool) error {
		if operation == "file-sync" || operation == "rename" || operation == "dir-sync" {
			writes++
			return fmt.Errorf("duplicate renewal must not persist again")
		}
		return nil
	}
	err = renewer.RenewActivation(ctx, f.activation.Digest(), next)
	j.files.hook = nil
	if err != nil || writes != 0 {
		t.Fatal("same original renewal was not an idempotent observation", writes, err)
	}
	if err := renewer.RenewActivation(ctx, strings.Repeat("f", 64), next); err == nil {
		t.Fatal("matching current digest cannot excuse a foreign predecessor")
	}

	// Advance only the trusted test clock. The old activation is expired under
	// the real verifier while a newly signed command is within the replacement's
	// live interval. This journal unit is not real elapsed-time/PID1 evidence.
	f.clock.now = f.activation.NotAfter().Add(2 * time.Second)
	if _, err := f.o.Verifier.VerifyTargetActivation(f.activation.Wire(), f.activation.RuntimeCertificate(), f.activation.IssuerCertificate(), f.activation.Birth(), f.clock.now); err == nil {
		t.Fatal("fixture did not cross the original activation cutoff")
	}
	claims := f.claims
	claims.Context.CommandID = "91111111-1111-4111-8111-111111111111"
	claims.Context.OperationID = "a1111111-1111-4111-8111-111111111111"
	claims.Context.ExpiresAt = next.NotAfter()
	claims.NotBefore = f.clock.now.Add(-2 * time.Second)
	claims.NotAfter = f.clock.now.Add(20 * time.Second)
	issuer, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := controlprotocol.SignExecStartTicket(f.issuerKey, issuer, claims)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := f.o.Verifier.VerifyExecStartTicket(ticket, f.issuerWire, claims.Context, f.descriptor, f.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.AcceptExecution(ctx, evidence); err != nil {
		t.Fatal("replacement must authorize a fresh journal acceptance past the old cutoff", err)
	}
	retained, err := j.Lookup(ctx, original.Context.CommandID)
	if err != nil || retained == nil || *retained != original {
		t.Fatal("renewal changed the already admitted command's original evidence", retained, err)
	}
}

func TestLiveJournalRenewActivationCanShorten(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	next := f.nextActivation(t, func(claims *controlprotocol.TargetActivationClaims, _ *controlprotocol.BirthContext) {
		claims.NotAfter = f.activation.NotAfter().Add(-time.Minute)
	})
	if err := requireJournalActivationRenewer(t, j).RenewActivation(ctx, f.activation.Digest(), next); err != nil {
		t.Fatal("later-issued activation may legally shorten business TTL", err)
	}
	if j.Status().Gate.GateState != "open" {
		t.Fatal("valid replacement unexpectedly closed the live journal")
	}
	wire, err := os.ReadFile(filepath.Join(f.o.Directory, "activation.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := decodeJournalActivation(wire)
	if err != nil || bundle.Digest != next.Digest() {
		t.Fatal("shortened activation was not persisted", err)
	}
}

func TestLiveJournalRenewActivationRefusesRebindingAndRollback(t *testing.T) {
	for _, kind := range []string{"previous", "gate", "birth", "closed", "rollback"} {
		t.Run(kind, func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			if err := j.InstallActivation(ctx, f.activation); err != nil {
				t.Fatal(err)
			}
			previous := f.activation.Digest()
			next := f.nextActivation(t, func(claims *controlprotocol.TargetActivationClaims, birth *controlprotocol.BirthContext) {
				switch kind {
				case "gate":
					claims.DataGateEpoch++
				case "birth":
					claims.UID++
					birth.UID++
				}
			})
			renewer := requireJournalActivationRenewer(t, j)
			switch kind {
			case "previous":
				previous = strings.Repeat("f", 64)
			case "closed":
				if _, err := j.CloseGate(ctx, f.o.DataGateEpoch); err != nil {
					t.Fatal(err)
				}
			case "rollback":
				if err := renewer.RenewActivation(ctx, previous, next); err != nil {
					t.Fatal(err)
				}
				previous = next.Digest()
				next = f.nextActivation(t, func(claims *controlprotocol.TargetActivationClaims, _ *controlprotocol.BirthContext) {
					claims.ActivationID = "b1111111-1111-4111-8111-111111111111"
					claims.NotBefore = f.activation.NotBefore().Add(5 * time.Second)
				})
			}
			before, err := os.ReadFile(filepath.Join(f.o.Directory, "activation.json"))
			if err != nil {
				t.Fatal(err)
			}
			status := j.Status()
			if err := renewer.RenewActivation(ctx, previous, next); err == nil {
				t.Fatal("renewal accepted a foreign binding, closed gate or older issuance")
			}
			after, err := os.ReadFile(filepath.Join(f.o.Directory, "activation.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refused renewal replaced durable activation", err)
			}
			current := j.Status()
			if current.Gate != status.Gate || current.Poisoned || !current.AccountingKnown || current.LogicalBytes != status.LogicalBytes {
				t.Fatal("refused renewal changed the gate or accounting", status, current)
			}
		})
	}
}

func TestLiveJournalRenewActivationPersistenceFailurePoisons(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	next := f.nextActivation(t, nil)
	renewer := requireJournalActivationRenewer(t, j)
	hit := false
	j.files.hook = func(operation, _ string, after bool) error {
		if operation == "dir-sync" && after {
			hit = true
			return fmt.Errorf("renewal directory sync acknowledgement lost")
		}
		return nil
	}
	err := renewer.RenewActivation(ctx, f.activation.Digest(), next)
	j.files.hook = nil
	status := j.Status()
	if !hit || err == nil || !status.Poisoned || status.AccountingKnown || status.Gate.GateState != "closed" {
		t.Fatal("uncertain durable replacement must poison and close admission", hit, err, status)
	}
	if err := renewer.RenewActivation(ctx, f.activation.Digest(), next); err == nil {
		t.Fatal("retry revived an uncertain journal replacement")
	}
	if _, err := j.AcceptExecution(ctx, f.e); err == nil {
		t.Fatal("uncertain renewal admitted a command")
	}
}

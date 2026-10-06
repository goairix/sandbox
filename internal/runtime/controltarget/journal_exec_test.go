//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

type diagnosticJournal interface {
	RecordUnknown(context.Context, controlprotocol.ExecStartEvidence) (*ExecJournalRecord, error)
	Lookup(context.Context, string) (*ExecJournalRecord, error)
}

func execJournalAPI(t *testing.T, j *Journal) diagnosticJournal {
	t.Helper()
	a, ok := any(j).(diagnosticJournal)
	if !ok {
		t.Fatal("Journal has no diagnostic RecordUnknown/Lookup API")
	}
	return a
}

// All evidence comes from the existing pinned-root verifier. The observation
// is deliberately historical: journal persistence never reauthorizes a start.
func journalEvidence(t *testing.T, r ExecJournalRecord, argument string) controlprotocol.ExecStartEvidence {
	t.Helper()
	rootKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	delegate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	c := r.Context
	v, err := controlprotocol.NewManagementVerifier(controlprotocol.TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}, []ed25519.PublicKey{rootKey.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	now := r.NotBefore.Add(2 * time.Second)
	cert := controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: c.IssuerCertificateID, Role: "command_issuer", Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, PublicKey: delegate.Public().(ed25519.PublicKey), NotBefore: r.NotBefore.Add(-time.Minute), NotAfter: r.NotAfter.Add(time.Minute)}
	issuerWire, err := controlprotocol.SignCommandIssuerCertificate(rootKey, cert)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerWire, now)
	if err != nil {
		t.Fatal(err)
	}
	c.IssuerCertificateDigest = issuer.Digest()
	d, err := controlprotocol.NewExecutionDescriptor(controlprotocol.ExecutionRequest{Argv: []string{"/bin/tool", argument}, UID: 1000, GID: 1000, WorkDir: "/work", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	claims := controlprotocol.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: d.Digest(), NotBefore: r.NotBefore, NotAfter: r.NotAfter}
	wire, err := controlprotocol.SignExecStartTicket(delegate, issuer, claims)
	if err != nil {
		t.Fatal(err)
	}
	e, err := v.VerifyExecStartTicket(wire, issuerWire, c, d, now)
	if err != nil {
		t.Fatal(err)
	}
	// Invalid signatures cannot provide a forged nonzero opaque value.
	wire[len(wire)-5] ^= 1
	forged, verifyErr := v.VerifyExecStartTicket(wire, issuerWire, c, d, now)
	if verifyErr == nil || len(forged.Wire()) != 0 {
		t.Fatal("invalid ticket produced evidence")
	}
	return e
}

func evidenceRecord(e controlprotocol.ExecStartEvidence) ExecJournalRecord {
	return ExecJournalRecord{Version: 1, State: "unknown", Context: e.Context(), DescriptorDigest: e.DescriptorDigest(), TicketDigest: e.Digest(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter()}
}

func commandPath(o JournalOptions, id string) string {
	return filepath.Join(o.Directory, "commands", id[:2], id+".json")
}

func TestJournalRecordUnknown(t *testing.T) {
	j, o := createJournalFixture(t)
	a := execJournalAPI(t, j)
	e := journalEvidence(t, journalRecordFixture(), "raw <&> $argument")
	want := evidenceRecord(e)
	got, err := a.RecordUnknown(context.Background(), e)
	if err != nil || got == nil || *got != want {
		t.Fatalf("first record %+v %v", got, err)
	}
	before, err := os.ReadFile(commandPath(o, want.Context.CommandID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte("raw <&> $argument")) || bytes.Contains(before, []byte("signature")) || bytes.Contains(before, []byte("claims")) {
		t.Fatal("ticket or execution payload persisted")
	}
	status := j.Status()
	if status.Gate.GateState != "closed" || status.Records != 1 || status.LogicalBytes != sizedGate(t, o)+int64(len(before)) {
		t.Fatalf("status %+v", status)
	}
	got.Context.Runtime.BootID = "result-mutated"
	got.NotAfter = got.NotAfter.Add(time.Second)
	inputWire := e.Wire()
	inputWire[0] ^= 1
	inputContext := e.Context()
	inputContext.RequestID = "input-mutated"
	for i := 0; i < 2; i++ {
		got, err = a.RecordUnknown(context.Background(), e)
		if err != nil || got == nil || *got != want || j.Status() != status {
			t.Fatalf("canonical retry %+v %v", got, err)
		}
		got.Context.RequestID = "retry-mutated"
	}
	got, err = a.Lookup(context.Background(), want.Context.CommandID)
	if err != nil || got == nil || *got != want {
		t.Fatalf("owned lookup %+v %v", got, err)
	}
	got.TicketDigest = strings.Repeat("f", 64)
	after, err := os.ReadFile(commandPath(o, want.Context.CommandID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("owned result changed disk", err)
	}
	t.Logf("historical authentic unknown record=%dB ticket=%dB gate=%dB; gate closed, no launch/replay", len(before), len(e.Wire()), sizedGate(t, o))
}

func TestJournalRecordUnknownConflict(t *testing.T) {
	j, o := createJournalFixture(t)
	a := execJournalAPI(t, j)
	r := journalRecordFixture()
	e := journalEvidence(t, r, "original")
	if _, err := a.RecordUnknown(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(commandPath(o, r.Context.CommandID))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*ExecJournalRecord){
		"issuer":             func(r *ExecJournalRecord) { r.Context.IssuerCertificateID = "11111111-1111-4111-8111-111111111112" },
		"operation":          func(r *ExecJournalRecord) { r.Context.OperationID = "33333333-3333-4333-8333-333333333334" },
		"request":            func(r *ExecJournalRecord) { r.Context.RequestID = "other" },
		"operation-digest":   func(r *ExecJournalRecord) { r.Context.OperationDigest = strings.Repeat("f", 64) },
		"control-revision":   func(r *ExecJournalRecord) { r.Context.ControlRevision++ },
		"admission-revision": func(r *ExecJournalRecord) { r.Context.AdmissionRevision++ },
		"lease":              func(r *ExecJournalRecord) { r.Context.LeaseID++ },
		"business-expiry":    func(r *ExecJournalRecord) { r.Context.ExpiresAt = r.Context.ExpiresAt.Add(time.Second) },
		"window-start":       func(r *ExecJournalRecord) { r.NotBefore = r.NotBefore.Add(time.Second) },
		"window-end":         func(r *ExecJournalRecord) { r.NotAfter = r.NotAfter.Add(-time.Second) },
		"extend-window": func(r *ExecJournalRecord) {
			r.NotBefore = r.NotBefore.Add(time.Second)
			r.NotAfter = r.NotAfter.Add(time.Second)
			r.Context.ExpiresAt = r.Context.ExpiresAt.Add(time.Second)
		},
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := r
			change(&candidate)
			other := journalEvidence(t, candidate, "original")
			if got, err := a.RecordUnknown(context.Background(), other); got != nil || !errors.Is(err, ErrConflict) {
				t.Fatalf("changed canonical record accepted %+v %v", got, err)
			}
		})
	}
	t.Run("descriptor-and-ticket-digests", func(t *testing.T) {
		other := journalEvidence(t, r, "other payload")
		if _, err := a.RecordUnknown(context.Background(), other); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	})
	after, err := os.ReadFile(commandPath(o, r.Context.CommandID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("conflict overwrote immutable record", err)
	}
}

func TestJournalRecordUnknownBinding(t *testing.T) {
	mutations := map[string]func(*ExecJournalRecord){
		"namespace": func(r *ExecJournalRecord) { r.Context.Namespace = "/sandbox/control/other/" }, "authority": func(r *ExecJournalRecord) { r.Context.AuthorityID = "other" }, "target": func(r *ExecJournalRecord) { r.Context.Target = "other" }, "restore": func(r *ExecJournalRecord) { r.Context.RestoreEpoch = "other" }, "sandbox": func(r *ExecJournalRecord) { r.Context.SandboxID = "other" }, "workspace": func(r *ExecJournalRecord) { r.Context.WorkspaceHash = strings.Repeat("f", 64) }, "generation": func(r *ExecJournalRecord) { r.Context.Generation++ }, "runtime-id": func(r *ExecJournalRecord) { r.Context.Runtime.ID = "other" }, "runtime-uid": func(r *ExecJournalRecord) { r.Context.Runtime.UID = "other" }, "boot": func(r *ExecJournalRecord) { r.Context.Runtime.BootID = "other" }, "epoch": func(r *ExecJournalRecord) { r.Context.DataGateEpoch++ },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			j, _ := createJournalFixture(t)
			a := execJournalAPI(t, j)
			r := journalRecordFixture()
			change(&r)
			e := journalEvidence(t, r, "payload")
			if got, err := a.RecordUnknown(context.Background(), e); got != nil || !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("binding %+v %v", got, err)
			}
			if j.Status().Records != 0 {
				t.Fatal("binding wrote history")
			}
		})
	}
}

func TestJournalExecHandles(t *testing.T) {
	e := journalEvidence(t, journalRecordFixture(), "payload")
	for _, j := range []*Journal{nil, {}} {
		a := execJournalAPI(t, j)
		for _, ctx := range []context.Context{nil, context.Background()} {
			want := ErrJournalUnavailable
			if ctx == nil {
				want = ErrInvalidConfiguration
			}
			if _, err := a.RecordUnknown(ctx, e); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if _, err := a.Lookup(ctx, e.Context().CommandID); !errors.Is(err, want) {
				t.Fatal(err)
			}
		}
	}
	j, _ := createJournalFixture(t)
	a := execJournalAPI(t, j)
	if _, err := a.RecordUnknown(context.Background(), controlprotocol.ExecStartEvidence{}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("zero evidence %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.RecordUnknown(ctx, e); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := a.Lookup(ctx, e.Context().CommandID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if j.Status().Poisoned {
		t.Fatal("pre-cancel poisoned")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecordUnknown(context.Background(), e); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := a.Lookup(context.Background(), e.Context().CommandID); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestJournalLookup(t *testing.T) {
	j, o := createJournalFixture(t)
	a := execJournalAPI(t, j)
	for _, id := range []string{"../gate.json", "", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"} {
		if _, err := a.Lookup(context.Background(), id); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("unsafe id %q: %v", id, err)
		}
	}
	missing := "33000000-0000-4000-8000-000000000001"
	if got, err := a.Lookup(context.Background(), missing); got != nil || err != nil {
		t.Fatalf("missing bucket %+v %v", got, err)
	}
	e := journalEvidence(t, journalRecordFixture(), "payload")
	if _, err := a.RecordUnknown(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	missing = "22000000-0000-4000-8000-000000000001"
	if got, err := a.Lookup(context.Background(), missing); got != nil || err != nil {
		t.Fatalf("missing file %+v %v", got, err)
	}
	for _, wire := range [][]byte{[]byte("{"), bytes.Repeat([]byte(" "), 8193)} {
		mustWrite(t, commandPath(o, e.Context().CommandID), wire)
		if got, err := a.Lookup(context.Background(), e.Context().CommandID); got != nil || !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("corrupt file hidden %+v %v", got, err)
		}
	}
	if j.Status().Poisoned {
		t.Fatal("diagnostic read poisoned")
	}
}

func TestJournalLookupDiskBinding(t *testing.T) {
	for _, which := range []string{"command-id", "boot", "symlink", "hardlink", "mode"} {
		t.Run(which, func(t *testing.T) {
			j, o := createJournalFixture(t)
			a := execJournalAPI(t, j)
			e := journalEvidence(t, journalRecordFixture(), "payload")
			got, err := a.RecordUnknown(context.Background(), e)
			if err != nil {
				t.Fatal(err)
			}
			path := commandPath(o, e.Context().CommandID)
			want := ErrInvalidRecord
			switch which {
			case "command-id":
				got.Context.CommandID = "22222222-2222-4222-8222-222222222223"
				b, _ := encodeExecJournalRecord(*got)
				mustWrite(t, path, b)
			case "boot":
				got.Context.Runtime.BootID = "other"
				b, _ := encodeExecJournalRecord(*got)
				mustWrite(t, path, b)
				want = ErrIdentityMismatch
			case "symlink":
				if err = os.Remove(path); err == nil {
					err = os.Symlink(filepath.Join(o.Directory, "gate.json"), path)
				}
			case "hardlink":
				err = os.Link(path, filepath.Join(o.Directory, "linked-record"))
			case "mode":
				err = os.Chmod(path, 0640)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := a.Lookup(context.Background(), e.Context().CommandID); got != nil || err == nil {
				t.Fatalf("unsafe file read %+v %v", got, err)
			} else if which != "symlink" && !errors.Is(err, want) {
				t.Fatal(fmt.Errorf("%s: %w", which, err))
			}
		})
	}
}

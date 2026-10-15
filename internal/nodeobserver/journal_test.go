package nodeobserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func TestNodeObserverJournal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := journalRecord{Version: 1, State: "enrolled", Enrollment: json.RawMessage(`{"version":1}`)}
	slot, err := j.reserve(r)
	if err != nil || slot != 0 {
		t.Fatalf("reserve slot=%d: %v", slot, err)
	}
	if _, err := j.reserve(r); err == nil {
		t.Fatal("journal reused an unresolved slot")
	}
	r.State, r.Intent, r.Ticket = "accepted", json.RawMessage(`{"version":1}`), json.RawMessage(`{"version":1}`)
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	r.State = "signal-issued"
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}
	j2, err := openJournal(dir, 1)
	if err != nil || j2.records[0].State != "signal-issued" {
		t.Fatalf("recovery: %v", err)
	}
	if err := j2.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slot-0000.a"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openJournal(dir, 1); err == nil {
		t.Fatal("corrupt generation accepted")
	}
}

type journalCutoffClock struct{ utc time.Time }

func (c *journalCutoffClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	return p.ClockObservation{UTC: c.utc, Uncertainty: time.Millisecond}, ctx.Err()
}

// This tests historical signature/persistence semantics only. Synthetic claims
// here are not kernel evidence and never count toward producer acceptance.
func TestNodeObserverJournalHistoricalFsyncCutoff(t *testing.T) {
	rootPublic, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	clock := &journalCutoffClock{utc: now}
	cutoff := now.Add(20 * time.Millisecond)
	binding := p.TrustBinding{Namespace: "/observer/test/v1/", AuthorityID: "test", Target: "node", RestoreEpoch: "restore"}
	node, boot := "da1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", "ea1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	certificate, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: "1a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Role: "node_observer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: node, NodeBootID: boot, PublicKey: public, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{rootPublic})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := v.VerifyNodeObserverCertificate(certificate, node, boot, now)
	if err != nil {
		t.Fatal(err)
	}
	current := p.TaskCloseDataContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: "2a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", IssuerCertificateDigest: strings.Repeat("a", 64), CommandID: "3a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskID: "4a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskDigest: strings.Repeat("b", 64), ClaimID: "5a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", WorkerID: "worker", SandboxID: "sandbox", WorkspaceHash: strings.Repeat("aa", 32), Generation: 1, DataGateEpoch: 1, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: p.RuntimeReference{ID: "pod", UID: "pod-uid", BootID: boot}}
	claims := p.NodeTerminationProofClaims{Version: 1, Purpose: "task_runtime_terminated", Current: current, Partition: 0xaa, IntentCreateRevision: 13, IntentDigest: strings.Repeat("c", 64), EnrollmentDigest: strings.Repeat("d", 64), ObserverCertificateDigest: identity.Digest(), TicketDigest: strings.Repeat("e", 64), NodeUID: node, NodeBootID: boot, PodUID: "pod-uid", PodResourceVersion: "17", ProviderDeletingAt: now.Add(-time.Second), PID1Dead: true, CgroupEmpty: true, ObservedAt: now}
	proof, err := p.SignNodeTerminationProof(key, identity, claims)
	if err != nil {
		t.Fatal(err)
	}
	clock.utc = cutoff.Add(time.Second)
	observation, err := clock.Observe(context.Background())
	if err != nil || !observation.UTC.After(cutoff) {
		t.Fatal("controlled clock did not cross original cutoff")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := journalRecord{Version: 1, State: "enrolled", Enrollment: json.RawMessage(`{"version":1}`)}
	if _, err := j.reserve(r); err != nil {
		t.Fatal(err)
	}
	r.State, r.Intent, r.Ticket = "accepted", json.RawMessage(`{"version":1}`), json.RawMessage(`{"version":1}`)
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	r.State = "signal-issued"
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	r.State, r.Terminal = "terminal", proof
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}
	j, err = openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	query := p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: 0xaa, TaskID: current.TaskID, CommandID: current.CommandID, IntentCreateRevision: 13, IntentDigest: claims.IntentDigest, EnrollmentDigest: claims.EnrollmentDigest}
	retained, err := v.VerifyNodeTerminationProof(j.records[0].Terminal, certificate, query)
	if err != nil || retained != claims || string(j.records[0].Terminal) != string(proof) {
		t.Fatalf("historical original proof was rewritten after real fsync: %v", err)
	}
}

func TestNodeObserverJournalBounds(t *testing.T) {
	for _, n := range []uint32{0, 1025} {
		if _, err := openJournal(t.TempDir(), n); err == nil {
			t.Fatal("invalid slot count accepted")
		}
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	if _, err := j.reserve(journalRecord{Version: 1, State: "terminal"}); err == nil {
		t.Fatal("terminal without original enrollment accepted")
	}
	if err := os.Symlink("/dev/null", filepath.Join(dir, "slot-0000.a")); err != nil {
		t.Fatal(err)
	}
	if _, err := openJournal(dir, 1); err == nil {
		t.Fatal("symlink generation accepted")
	}
}

func TestNodeObserverJournalCrashAndTransitions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := journalRecord{Version: 1, State: "enrolled", Enrollment: json.RawMessage(`{"version":1}`)}
	if _, err := j.reserve(r); err != nil {
		t.Fatal(err)
	}
	r.State, r.Intent, r.Ticket = "signal-issued", json.RawMessage(`{"version":1}`), json.RawMessage(`{"version":1}`)
	if err := j.put(0, r); err == nil {
		t.Fatal("signal before accepted journal state")
	}
	r.State = "accepted"
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}
	// Real unpublished torn generation: restart retains accepted and cannot
	// reinterpret temporary bytes as signal-issued or terminal.
	if err := os.Remove(filepath.Join(dir, "slot-0000.a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "next-0000"), []byte(`{"sequence":3`), 0600); err != nil {
		t.Fatal(err)
	}
	j, err = openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	if j.records[0].State != "accepted" {
		t.Fatal("torn generation changed accepted attempt")
	}
	r.Intent = json.RawMessage(`{"version":2}`)
	r.State = "signal-issued"
	if err := j.put(0, r); err == nil {
		t.Fatal("attempt bytes were replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "next-0000")); !os.IsNotExist(err) {
		t.Fatal("torn temporary not removed and fsynced")
	}
}

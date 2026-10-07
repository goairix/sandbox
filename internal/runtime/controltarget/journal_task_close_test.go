//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func taskCloseClaims(f liveFixture) controlprotocol.TaskCloseDataTicketClaims {
	c := f.claims.Context
	return controlprotocol.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: controlprotocol.TaskCloseDataContext{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, IssuerCertificateID: c.IssuerCertificateID, IssuerCertificateDigest: c.IssuerCertificateDigest, CommandID: "81111111-1111-4111-8111-111111111111", TaskID: "91111111-1111-4111-8111-111111111111", TaskDigest: strings.Repeat("b", 64), ClaimID: "a1111111-1111-4111-8111-111111111111", WorkerID: "worker-1", SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, DataGateEpoch: c.DataGateEpoch, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: c.Runtime}, NotBefore: f.clock.now.Add(-2 * time.Second), NotAfter: f.clock.now.Add(20 * time.Second)}
}
func taskCloseEvidence(t *testing.T, f liveFixture, c controlprotocol.TaskCloseDataTicketClaims) controlprotocol.TaskCloseDataEvidence {
	t.Helper()
	i, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
	require.NoError(t, err)
	w, err := controlprotocol.SignTaskCloseDataTicket(f.issuerKey, i, c)
	require.NoError(t, err)
	e, err := f.o.Verifier.VerifyTaskCloseDataTicket(w, f.issuerWire, c.Context, f.clock.now)
	require.NoError(t, err)
	return e
}

// A complete retained pending record must be diagnosed and counted on cold
// recovery, without becoming close proof or recovering activation authority.
func TestJournalTaskCloseColdPending(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	_, err := j.CloseGate(ctx, f.o.DataGateEpoch)
	require.NoError(t, err)
	before := j.Status()
	require.NoError(t, j.Close())
	c := taskCloseClaims(f)
	e := taskCloseEvidence(t, f, c)
	wire, err := json.Marshal(map[string]any{"version": 1, "state": "pending", "context": c.Context, "ticket_digest": e.Digest(), "not_before": c.NotBefore, "not_after": c.NotAfter})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, "data-close.json"), wire, 0600))
	cold, err := OpenClosedJournal(ctx, f.o)
	require.NoError(t, err)
	history, err := cold.LookupDataClose(ctx, e.Context(), e.Digest())
	require.NoError(t, err)
	require.NotNil(t, history)
	require.Equal(t, "pending", history.State)
	denied, err := cold.CloseData(ctx, e)
	require.Error(t, err)
	require.Nil(t, denied)
	require.NoError(t, cold.Close())
	status := cold.Status()
	require.Equal(t, "closed", status.Gate.GateState)
	require.Equal(t, before.LogicalBytes+int64(len(wire)), status.LogicalBytes)
	require.Equal(t, before.Records, status.Records)
}

func TestJournalTaskCloseLifecycle(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	a, err := j.AcceptExecution(ctx, f.e)
	require.NoError(t, err)
	execBefore, err := j.Lookup(ctx, f.e.Context().CommandID)
	require.NoError(t, err)
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	var writes []string
	j.files.hook = func(op, name string, after bool) error {
		if op == "rename" && after {
			writes = append(writes, name)
		}
		return nil
	}
	r, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.NotNil(t, r)
	require.Equal(t, []string{"data-close.json", "gate.json", "data-close.json"}, writes)
	require.Equal(t, "data_closed", r.State)
	require.Equal(t, e.Context(), r.Context)
	require.Equal(t, e.Digest(), r.TicketDigest)
	require.Equal(t, "closed", j.Status().Gate.GateState)
	execAfter, err := j.Lookup(ctx, f.e.Context().CommandID)
	require.NoError(t, err)
	require.Equal(t, execBefore, execAfter)
	_, err = a.Snapshot()
	require.NoError(t, err)
	require.Error(t, j.InstallActivation(ctx, f.activation))
	before := j.Status()
	r.Context.WorkerID = "caller-mutation"
	writes = nil
	retry, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.Equal(t, e.Context(), retry.Context)
	require.Empty(t, writes)
	require.Equal(t, before, j.Status())
	c := taskCloseClaims(f)
	c.Context.ClaimID = "b1111111-1111-4111-8111-111111111111"
	got, err := j.CloseData(ctx, taskCloseEvidence(t, f, c))
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, got)
	j.files.hook = nil
	require.NoError(t, j.Close())
	f.clock.now = f.clock.now.Add(2 * time.Hour)
	cold, err := OpenClosedJournal(ctx, f.o)
	require.NoError(t, err)
	defer func() { require.NoError(t, cold.Close()) }()
	calls := f.clock.calls
	history, err := cold.LookupDataClose(ctx, e.Context(), e.Digest())
	require.NoError(t, err)
	require.Equal(t, retry, history)
	require.Equal(t, calls, f.clock.calls)
	got, err = cold.CloseData(ctx, e)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestJournalTaskCloseExpiredActivation(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	// Both old business expiry (10m) and activation expiry (30m) have passed;
	// the original installed certificates remain fresh for one hour.
	f.clock.now = f.clock.now.Add(40 * time.Minute)
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	r, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.Equal(t, "data_closed", r.State)
}

func TestJournalTaskCloseAuthorization(t *testing.T) {
	for _, kind := range []string{"zero", "copy", "cold", "activation", "birth-uid", "birth-key", "runtime-boot", "gate", "restore", "issuer", "expired", "clock"} {
		t.Run(kind, func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			require.NoError(t, j.InstallActivation(ctx, f.activation))
			c := taskCloseClaims(f)
			e := taskCloseEvidence(t, f, c)
			switch kind {
			case "zero":
				e = controlprotocol.TaskCloseDataEvidence{}
			case "copy":
				var copied Journal
				reflect.ValueOf(&copied).Elem().Set(reflect.ValueOf(j).Elem())
				j = &copied
			case "cold":
				j.fresh = false
			case "activation":
				j.activation = nil
			case "birth-uid":
				j.birth.UID++
			case "birth-key":
				j.birth.RuntimePublicKey[0] ^= 1
			case "runtime-boot":
				c.Context.Runtime.BootID = "other"
				e = taskCloseEvidence(t, f, c)
			case "gate":
				c.Context.DataGateEpoch++
				e = taskCloseEvidence(t, f, c)
			case "restore":
				j.gate.Identity.RestoreEpoch = "other"
			case "issuer":
				other := liveSetup(t)
				e = taskCloseEvidence(t, other, taskCloseClaims(other))
			case "expired":
				f.clock.now = e.NotAfter()
			case "clock":
				f.clock.err = fmt.Errorf("clock uncertainty")
			}
			writes := 0
			j.files.hook = func(op, name string, after bool) error {
				if op == "open-temp" {
					writes++
				}
				return nil
			}
			r, err := j.CloseData(ctx, e)
			require.Error(t, err)
			require.Nil(t, r)
			require.Zero(t, writes)
			_, err = os.Stat(filepath.Join(f.o.Directory, "data-close.json"))
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestJournalTaskCloseConflict(t *testing.T) {
	for _, kind := range []string{"command", "claim", "task", "worker", "window"} {
		t.Run(kind, func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			require.NoError(t, j.InstallActivation(ctx, f.activation))
			c := taskCloseClaims(f)
			e := taskCloseEvidence(t, f, c)
			first, err := j.CloseData(ctx, e)
			require.NoError(t, err)
			switch kind {
			case "command":
				c.Context.CommandID = "b1111111-1111-4111-8111-111111111111"
			case "claim":
				c.Context.ClaimID = "b1111111-1111-4111-8111-111111111111"
			case "task":
				c.Context.TaskID = "b1111111-1111-4111-8111-111111111111"
			case "worker":
				c.Context.WorkerID = "other"
			case "window":
				c.NotAfter = c.NotAfter.Add(-time.Second)
			}
			r, err := j.CloseData(ctx, taskCloseEvidence(t, f, c))
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, r)
			r, err = j.LookupDataClose(ctx, e.Context(), e.Digest())
			require.NoError(t, err)
			require.Equal(t, first, r)
			_, err = j.LookupDataClose(ctx, e.Context(), strings.Repeat("f", 64))
			require.ErrorIs(t, err, ErrConflict)
		})
	}
}

func TestJournalTaskCloseCodec(t *testing.T) {
	f := liveSetup(t)
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	r := TaskDataCloseRecord{Version: 1, State: "pending", Context: e.Context(), TicketDigest: e.Digest(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter()}
	wire, err := encodeTaskDataClose(r)
	require.NoError(t, err)
	// Field edits use independent JSON objects, not production schemas.
	var visit func([]byte, func([]byte) []byte, string)
	visit = func(object []byte, embed func([]byte) []byte, path string) {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(object, &fields))
		for field, value := range fields {
			for _, kind := range []string{"missing", "null", "duplicate"} {
				t.Run(path+field+"/"+kind, func(t *testing.T) {
					var m map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(object, &m))
					m[field] = json.RawMessage("null")
					if kind == "missing" {
						delete(m, field)
					}
					bad, err := json.Marshal(m)
					require.NoError(t, err)
					if kind == "duplicate" {
						key, _ := json.Marshal(field)
						bad = append(append(append(append([]byte{'{'}, key...), ':'), value...), ',')
						bad = append(bad, object[1:]...)
					}
					got, err := decodeTaskDataClose(embed(bad))
					require.Error(t, err)
					require.Equal(t, TaskDataCloseRecord{}, got)
				})
			}
			if len(value) > 0 && value[0] == '{' {
				visit(value, func(inner []byte) []byte {
					var m map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(object, &m))
					m[field] = inner
					b, err := json.Marshal(m)
					require.NoError(t, err)
					return embed(b)
				}, path+field+"/")
			}
		}
		t.Run(path+"unknown", func(t *testing.T) {
			bad := append([]byte(`{"drain_confirmed":true,`), object[1:]...)
			_, err := decodeTaskDataClose(embed(bad))
			require.Error(t, err)
		})
	}
	visit(wire, func(b []byte) []byte { return b }, "")
	for _, bad := range [][]byte{append(bytes.Clone(wire), 'x'), append(bytes.Clone(wire), 255), bytes.Replace(wire, []byte(`"lease_id":12`), []byte(`"lease_id":1.5`), 1), bytes.Replace(wire, []byte(`"pending"`), []byte(`"drained"`), 1), bytes.Replace(wire, []byte(`"worker-1"`), []byte(`"\ud800"`), 1), bytes.Replace(wire, []byte(`"claim_create_revision":11`), []byte(`"claim_create_revision":10`), 1), bytes.Replace(wire, []byte(`Z"`), []byte(`+00:00"`), 1)} {
		got, err := decodeTaskDataClose(bad)
		require.Error(t, err)
		require.Equal(t, TaskDataCloseRecord{}, got)
	}
	unicodeRecord := r
	unicodeRecord.Context.Runtime.UID = "replacement-�"
	unicodeWire, err := encodeTaskDataClose(unicodeRecord)
	require.NoError(t, err)
	decoded, err := decodeTaskDataClose(unicodeWire)
	require.NoError(t, err)
	require.Equal(t, unicodeRecord, decoded)
	for _, invalid := range [][]byte{[]byte(`\ud800`), []byte(`\udc00`), {255}} {
		got, err := decodeTaskDataClose(bytes.Replace(unicodeWire, []byte("�"), invalid, 1))
		require.Error(t, err)
		require.Equal(t, TaskDataCloseRecord{}, got)
	}
	padded := append(bytes.Clone(wire), bytes.Repeat([]byte(" "), 8192-len(wire))...)
	got, err := decodeTaskDataClose(padded)
	require.NoError(t, err)
	require.Equal(t, r, got)
	got, err = decodeTaskDataClose(append(padded, ' '))
	require.Error(t, err)
	require.Equal(t, TaskDataCloseRecord{}, got)
}

func TestJournalTaskCloseProtectedHistory(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "mode", "oversize", "wrong-identity", "open-gate", "missing-activation"} {
		t.Run(kind, func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			require.NoError(t, j.InstallActivation(ctx, f.activation))
			e := taskCloseEvidence(t, f, taskCloseClaims(f))
			r, err := j.CloseData(ctx, e)
			require.NoError(t, err)
			require.NoError(t, j.Close())
			path := filepath.Join(f.o.Directory, "data-close.json")
			switch kind {
			case "symlink":
				require.NoError(t, os.Rename(path, filepath.Join(filepath.Dir(f.o.Directory), "retained-close")))
				require.NoError(t, os.Symlink(filepath.Join(filepath.Dir(f.o.Directory), "retained-close"), path))
			case "hardlink":
				require.NoError(t, os.Link(path, filepath.Join(filepath.Dir(f.o.Directory), "retained-close")))
			case "mode":
				require.NoError(t, os.Chmod(path, 0644))
			case "oversize":
				require.NoError(t, os.WriteFile(path, make([]byte, 8193), 0600))
			case "wrong-identity":
				r.Context.Runtime.BootID = "other"
				b, err := encodeTaskDataClose(*r)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, b, 0600))
			case "open-gate":
				g := j.gate
				g.GateState = "open"
				b, err := encodeJournalWire(g)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, "gate.json"), b, 0600))
			case "missing-activation":
				require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "activation.json")))
			}
			cold, err := OpenClosedJournal(ctx, f.o)
			if cold != nil {
				require.NoError(t, cold.Close())
			}
			require.Error(t, err)
		})
	}
}

func TestJournalTaskCloseRuntimeCertificate(t *testing.T) {
	for _, kind := range []string{"expired", "ticket-outside"} {
		t.Run(kind, func(t *testing.T) {
			f := liveSetup(t)
			now := f.clock.now
			binding := f.activation.Binding()
			identity := f.activation.Identity()
			birth := *f.o.Birth
			root, key, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			v, err := controlprotocol.NewManagementVerifier(binding, []ed25519.PublicKey{root})
			require.NoError(t, err)
			issuerClaims := controlprotocol.CommandIssuerCertificateClaims{Version: 1, Role: "command_issuer", CertificateID: f.claims.Context.IssuerCertificateID, Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: ed25519.PublicKey(f.issuerKey[32:]), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
			iw, err := controlprotocol.SignCommandIssuerCertificate(key, issuerClaims)
			require.NoError(t, err)
			runtimeAfter := now.Add(35 * time.Minute)
			activationAfter := now.Add(30 * time.Minute)
			if kind == "ticket-outside" {
				runtimeAfter = now.Add(15 * time.Second)
				activationAfter = now.Add(10 * time.Second)
			}
			rw, err := controlprotocol.SignRuntimeIdentityCertificate(key, controlprotocol.RuntimeIdentityCertificateClaims{Version: 1, Role: "runtime_receipt", CertificateID: "51111111-1111-4111-8111-111111111111", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: now.Add(-time.Minute), NotAfter: runtimeAfter, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime})
			require.NoError(t, err)
			aw, err := controlprotocol.SignTargetActivation(key, controlprotocol.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: "71111111-1111-4111-8111-111111111111", Binding: binding, Identity: identity, DataGateEpoch: f.o.DataGateEpoch, UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: digestJournalBytes(rw), IssuerCertificateDigest: digestJournalBytes(iw), NotBefore: now.Add(-time.Minute), NotAfter: activationAfter})
			require.NoError(t, err)
			activation, err := v.VerifyTargetActivation(aw, rw, iw, birth, now)
			require.NoError(t, err)
			f.o.Verifier = v
			f.issuerWire = iw
			f.claims.Context.IssuerCertificateDigest = digestJournalBytes(iw)
			j := f.create(t)
			require.NoError(t, j.InstallActivation(context.Background(), activation))
			if kind == "expired" {
				f.clock.now = now.Add(40 * time.Minute)
			}
			e := taskCloseEvidence(t, f, taskCloseClaims(f))
			r, err := j.CloseData(context.Background(), e)
			require.Error(t, err)
			require.Nil(t, r)
			require.True(t, j.Status().Poisoned)
			_, err = os.Stat(filepath.Join(f.o.Directory, "data-close.json"))
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestJournalTaskCloseNoProofFromGate(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	absent, err := j.LookupDataClose(ctx, e.Context(), e.Digest())
	require.NoError(t, err)
	require.Nil(t, absent)
	denied, err := j.CloseData(ctx, e)
	require.Error(t, err)
	require.Nil(t, denied)
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	_, err = j.CloseGate(ctx, f.o.DataGateEpoch)
	require.NoError(t, err)
	absent, err = j.LookupDataClose(ctx, e.Context(), e.Digest())
	require.NoError(t, err)
	require.Nil(t, absent)
	closed, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.Equal(t, "data_closed", closed.State)
}

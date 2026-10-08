package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

type unusedArchiveIssuer struct{}

func (*unusedArchiveIssuer) Certificate(context.Context) ([]byte, error) {
	panic("constructor invoked archive certificate")
}
func (*unusedArchiveIssuer) SignWorkspaceArchive(context.Context, string, p.TaskWorkspaceArchiveTicketClaims) ([]byte, error) {
	panic("constructor invoked archive signer")
}

func TestTaskArchiveAuthorityConfiguration(t *testing.T) {
	o := validOptions(t)
	v, err := taskWorkspaceArchiveAuthority(o)
	require.NoError(t, err)
	require.Nil(t, v)
	for _, partial := range []string{"trust-only", "clock-only"} {
		o := validOptions(t)
		if partial == "trust-only" {
			o.PublicationTrust = &RuntimePublicationTrust{}
		} else {
			o.Clock = testAuthorityClock(nil)
		}
		v, err := taskWorkspaceArchiveAuthority(o)
		require.NoError(t, err)
		require.Nil(t, v)
	}
	for _, fault := range []string{"valid", "query-only", "typednil", "trust", "clock", "typedclock", "roots"} {
		t.Run(fault, func(t *testing.T) {
			o := validOptions(t)
			o.TaskWorkspaceArchiveIssuer = &unusedArchiveIssuer{}
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
			o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("constructor invoked clock") })
			switch fault {
			case "query-only":
				o.TaskWorkspaceArchiveIssuer = nil
			case "typednil":
				o.TaskWorkspaceArchiveIssuer = (*unusedArchiveIssuer)(nil)
			case "trust":
				o.PublicationTrust = nil
			case "clock":
				o.Clock = nil
			case "typedclock":
				o.Clock = testAuthorityClock(nil)
			case "roots":
				o.PublicationTrust.Roots = nil
			}
			v, err := taskWorkspaceArchiveAuthority(o)
			if fault == "valid" || fault == "query-only" {
				require.NoError(t, err)
				require.NotNil(t, v)
				require.NoError(t, validateOptions(o))
			} else {
				require.ErrorIs(t, err, ErrInvalidConfiguration)
				require.Nil(t, v)
				require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
			}
		})
	}
}

func archiveCommandRecordFixture(t *testing.T) taskArchiveCommandRecord {
	t.Helper()
	q := quiesceRecordFixture(t)
	f := TaskFinalSyncAttemptRecord{Version: 1, Task: q.Task, Claim: q.Claim, DestroyingRevision: q.Context.Current.ControlRevision, Quiescence: TaskQuiescenceReference{Task: q.Task.Reference, CommandID: q.Context.Current.CommandID, Stage: q.Attempt.reference(strings.Repeat("a", 64))}, QuiescenceRevision: 50, Plan: finalSyncPlan(), AttemptID: uuid.NewString()}
	now := time.Now().UTC()
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	qc := p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: q.Context, TicketDigest: q.TicketDigest, NotBefore: now.Add(-time.Second), NotAfter: now.Add(20 * time.Second), ExecutionSetDigest: strings.Repeat("b", 64)}
	qw, err := p.SignTaskUserQuiescenceReceipt(key, qc)
	require.NoError(t, err)
	f.ReceiptDigest = digestTaskQuiescenceProof(qw)
	c := q.Context.Current
	c.CommandID = f.AttemptID
	c.IssuerCertificateID = uuid.NewString()
	ic := p.CommandIssuerCertificateClaims{Version: 1, Role: "command_issuer", CertificateID: c.IssuerCertificateID, Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	iw, err := p.SignCommandIssuerCertificate(root, ic)
	require.NoError(t, err)
	c.IssuerCertificateDigest = digestTaskQuiescenceProof(iw)
	v, err := p.NewManagementVerifier(p.TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	i, err := v.VerifyCommandIssuerCertificate(iw, now)
	require.NoError(t, err)
	a := p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: p.TaskWorkspaceArchiveContext{Current: c, QuiescenceContext: q.Context, QuiescenceTicketDigest: q.TicketDigest, QuiescenceReceiptDigest: f.ReceiptDigest, PlanDigest: f.Plan.Digest, QuiescenceIntentRevision: 50, FinalSyncRevision: 51}, NotBefore: now.Add(-time.Second), NotAfter: now.Add(20 * time.Second), SessionNotAfter: now.Add(44 * time.Second)}
	w, err := p.SignTaskWorkspaceArchiveTicket(key, i, a)
	require.NoError(t, err)
	return taskArchiveCommandRecord{Version: 1, FinalSync: taskFinalSyncWire(f), FinalSyncRevision: 51, Claims: a, IssuerRevision: 40, Certificate: iw, Ticket: w, QuiescenceReceipt: qw}
}

func TestTaskArchiveCommandStrictCodec(t *testing.T) {
	r := archiveCommandRecordFixture(t)
	w, err := encodeTaskArchiveCommand(r)
	require.NoError(t, err)
	ns := namespaceFromTaskCloseTest(t, r.FinalSync.Task.Reference)
	key, err := ns.taskWorkspaceArchiveKey(r.FinalSync.Task.Reference)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(w), Version: 1, CreateRevision: 52, ModRevision: 52}
	var got taskArchiveCommandRecord
	require.NoError(t, decodeTaskArchiveCommand(kv, &got))
	require.Equal(t, r, got)
	for _, fault := range []string{"unknown", "duplicate", "nested-unknown", "nested-null", "ticket-duplicate", "certificate-unknown", "receipt-null", "noncanonical", "reordered-ticket", "oversize", "key", "birth", "issuer-birth", "version", "lease", "mod", "command", "plan", "revision", "receipt-digest", "session"} {
		t.Run(fault, func(t *testing.T) {
			v := *kv
			v.Value = bytes.Clone(kv.Value)
			x := r
			switch fault {
			case "unknown":
				v.Value = append([]byte(`{"unknown":true,`), v.Value[1:]...)
			case "duplicate":
				v.Value = append([]byte(`{"version":1,`), v.Value[1:]...)
			case "nested-unknown":
				v.Value = []byte(strings.Replace(w, `"claims":{`, `"claims":{"unknown":1,`, 1))
			case "nested-null":
				v.Value = []byte(strings.Replace(w, `"purpose":"task_workspace_archive"`, `"purpose":null`, 1))
			case "ticket-duplicate":
				x.Ticket = append([]byte(`{"claims":{},`), x.Ticket[1:]...)
			case "certificate-unknown":
				x.Certificate = append([]byte(`{"unknown":true,`), x.Certificate[1:]...)
			case "receipt-null":
				x.QuiescenceReceipt = []byte("null")
			case "noncanonical":
				v.Value = append(v.Value, ' ')
			case "reordered-ticket":
				var m map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(x.Ticket, &m))
				x.Ticket = []byte(`{"signature":` + string(m["signature"]) + `,"claims":` + string(m["claims"]) + `}`)
			case "oversize":
				v.Value = bytes.Repeat([]byte("x"), maxRecordBytes+1)
			case "key":
				v.Key = append(bytes.Clone(v.Key), 'x')
			case "birth":
				v.CreateRevision = 51
				v.ModRevision = 51
			case "issuer-birth":
				x.IssuerRevision = 52
			case "version":
				v.Version++
			case "lease":
				v.Lease = 1
			case "mod":
				v.ModRevision++
			case "command":
				x.Claims.Context.Current.CommandID = uuid.NewString()
			case "plan":
				x.Claims.Context.PlanDigest = strings.Repeat("a", 64)
			case "revision":
				x.FinalSyncRevision++
			case "receipt-digest":
				x.FinalSync.ReceiptDigest = strings.Repeat("a", 64)
			case "session":
				x.Claims.SessionNotAfter = x.Claims.NotBefore.Add(time.Hour + time.Nanosecond)
			}
			if fault == "ticket-duplicate" || fault == "certificate-unknown" || fault == "receipt-null" || fault == "reordered-ticket" || fault == "issuer-birth" || fault == "command" || fault == "plan" || fault == "revision" || fault == "receipt-digest" || fault == "session" {
				v.Value, err = json.Marshal(x)
				require.NoError(t, err)
			}
			got = r
			require.ErrorIs(t, decodeTaskArchiveCommand(&v, &got), ErrCorruptRecord)
			require.Equal(t, taskArchiveCommandRecord{}, got)
		})
	}
	require.Error(t, decodeTaskArchiveCommand(kv, nil))
}

func TestTaskArchiveCommandRejectsBeforeIO(t *testing.T) {
	b := new(Backend)
	for _, claim := range []*TaskClaim{nil, {}} {
		r, err := b.PrepareTaskWorkspaceArchive(context.Background(), claim, &PreparedTaskQuiescence{}, nil, time.Minute)
		require.ErrorIs(t, err, ErrInvalidRecord)
		require.Nil(t, r.Prepared)
	}
	require.False(t, b.validPreparedTaskWorkspaceArchive(nil))
	require.False(t, b.validPreparedTaskWorkspaceArchive(&PreparedTaskWorkspaceArchive{}))
}

func TestTaskArchiveCommandProtobufBudget(t *testing.T) {
	r := archiveCommandRecordFixture(t)
	ns := namespaceFromTaskCloseTest(t, r.FinalSync.Task.Reference)
	b := &Backend{namespace: ns, identityValue: "identity", restoreEpoch: "restore"}
	b.identityKey, _ = ns.Key("identity")
	b.restoreKey, _ = ns.Key("restore")
	key, err := ns.taskWorkspaceArchiveKey(r.FinalSync.Task.Reference)
	require.NoError(t, err)
	cmps := make([]clientv3.Cmp, 58)
	for i := range cmps {
		cmps[i] = clientv3.Compare(clientv3.ModRevision(b.identityKey), "=", 1)
	}
	require.NoError(t, b.preflightTaskArchive(cmps, key, strings.Repeat("x", maxRecordBytes)))
	cmps = append(cmps, cmps[0])
	require.ErrorIs(t, b.preflightTaskArchive(cmps, key, ""), ErrInvalidMutation, "both read branches must count")
	require.ErrorIs(t, b.preflightTaskArchive(cmps[:1], key, strings.Repeat("x", maxRecordBytes+1)), ErrInvalidMutation)
	for i := range cmps[:4] {
		cmps[i] = clientv3.Compare(clientv3.Value(b.identityKey), "=", strings.Repeat("x", maxRecordBytes))
	}
	require.ErrorIs(t, b.preflightTaskArchive(cmps[:4], key, ""), ErrInvalidMutation, "actual protobuf bytes include comparison values")
}

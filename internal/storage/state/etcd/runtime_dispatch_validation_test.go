package etcd

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

func TestRuntimeDispatchDeclareRejectsBeforeGrant(t *testing.T) {
	b, raw, _, c := claimFixture(t)
	fresh := freshDispatchBackend(t, b, raw)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	declarer, ok := any(b).(runtimeDeclarer)
	require.True(t, ok)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name  string
		ctx   context.Context
		claim *CreationClaim
		input RuntimeDispatchInput
		want  error
	}{
		{"nil context", nil, c, dispatchInputForDeclaration(), ErrInvalidRecord},
		{"canceled", canceled, c, dispatchInputForDeclaration(), context.Canceled},
		{"nil claim", context.Background(), nil, dispatchInputForDeclaration(), ErrInvalidRecord},
		{"bad kind", context.Background(), c, RuntimeDispatchInput{Kind: "delete", Target: "docker", Payload: json.RawMessage(`{}`)}, ErrInvalidRecord},
		{"bad target", context.Background(), c, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: strings.Repeat("x", 129), Payload: json.RawMessage(`{}`)}, ErrInvalidRecord},
		{"malformed", context.Background(), c, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "docker", Payload: json.RawMessage(`{`)}, ErrInvalidRecord},
		{"wire budget", context.Background(), c, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "docker", Payload: json.RawMessage(`"` + strings.Repeat("x", 65500) + `"`)}, ErrInvalidRecord},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, e := declarer.DeclareRuntimeDispatch(tt.ctx, tt.claim, tt.input)
			require.ErrorIs(t, e, tt.want)
			require.Nil(t, r.Entry)
			require.Equal(t, OutcomeUnknown, r.Outcome)
		})
	}
	foreign, ok := any(fresh).(runtimeDeclarer)
	require.True(t, ok)
	r, e := foreign.DeclareRuntimeDispatch(context.Background(), c, dispatchInputForDeclaration())
	require.ErrorIs(t, e, ErrInvalidRecord)
	require.Nil(t, r.Entry)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
	r, e = declarer.DeclareRuntimeDispatch(context.Background(), c, dispatchInputForDeclaration())
	require.ErrorIs(t, e, ErrGuardExpired)
	require.Nil(t, r.Entry)
	require.Zero(t, lease.grants.Load())
}

func TestRuntimeDispatchDeclareRejectsMisalignedReplay(t *testing.T) {
	changes := map[string]func(*RuntimeDispatchRecord, *RuntimeDispatchInputRecord){
		"request hash": func(r *RuntimeDispatchRecord, _ *RuntimeDispatchInputRecord) { r.RequestHash = strings.Repeat("f", 64) },
		"configuration": func(r *RuntimeDispatchRecord, _ *RuntimeDispatchInputRecord) {
			r.ConfigurationDigest = strings.Repeat("f", 64)
		},
		"request id": func(r *RuntimeDispatchRecord, _ *RuntimeDispatchInputRecord) { r.Attempt.RequestID = "other-request" },
		"sandbox": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord) {
			r.SandboxID = "other-sandbox"
			i.SandboxID = r.SandboxID
		},
		"generation": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord) {
			r.Generation++
			i.Generation = r.Generation
		},
		"gate":     func(r *RuntimeDispatchRecord, _ *RuntimeDispatchInputRecord) { r.DataGateEpoch++ },
		"snapshot": func(r *RuntimeDispatchRecord, _ *RuntimeDispatchInputRecord) { r.Snapshot.Version = "other-snapshot" },
		"expiry": func(r *RuntimeDispatchRecord, _ *RuntimeDispatchInputRecord) {
			r.ExpiresAt = r.ExpiresAt.Add(time.Hour)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			b, raw, acquire, c := claimFixture(t)
			ctx := context.Background()
			input := dispatchInputForDeclaration()
			original, err := declareDispatch(t, b, c, input)
			require.NoError(t, err)
			dk, ik, err := b.namespace.runtimeDispatchKeys(acquire.Workspace.Partition(), acquire.IntentID)
			require.NoError(t, err)
			_, oldReceipt, err := b.stageKeys(original.Reference)
			require.NoError(t, err)
			r := original.Entry.Record
			i := RuntimeDispatchInputRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, OperationID: r.OperationID, PayloadDigest: r.PayloadDigest, Generation: r.Generation, Payload: original.Entry.Payload}
			change(&r, &i)
			alteredRef := r.Attempt.reference(original.Reference.Digest)
			_, rk, err := b.stageKeys(alteredRef)
			require.NoError(t, err)
			wire, err := encodeDomainRecord(r)
			require.NoError(t, err)
			body, err := encodeDomainRecord(i)
			require.NoError(t, err)
			receipt, err := encodeReceipt(alteredRef, OutcomeCommitted)
			require.NoError(t, err)
			_, err = raw.Txn(ctx).Then(clientv3.OpDelete(dk), clientv3.OpDelete(ik), clientv3.OpDelete(oldReceipt)).Commit()
			require.NoError(t, err)
			_, err = raw.Txn(ctx).Then(clientv3.OpPut(dk, wire), clientv3.OpPut(ik, body), clientv3.OpPut(rk, receipt)).Commit()
			require.NoError(t, err)
			// The public loader accepts this internally coherent bundle. Declaration must
			// additionally bind it to the original claim's validated request and control.
			loaded, err := b.LoadRuntimeDispatch(ctx, acquire.Workspace, acquire.IntentID)
			require.NoError(t, err)
			require.NotNil(t, loaded)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := declareDispatch(t, b, c, input)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Nil(t, result.Entry)
			require.Zero(t, lease.grants.Load())
			unchanged, err := raw.Get(ctx, dk)
			require.NoError(t, err)
			require.Equal(t, wire, string(unchanged.Kvs[0].Value))
			require.NoError(t, b.ReleaseCreationClaim(ctx, c))
		})
	}
}
func TestRuntimeDispatchDeclareRejectsCorruptExistingBundle(t *testing.T) {
	for _, part := range []string{"input", "receipt"} {
		t.Run(part, func(t *testing.T) {
			b, raw, acquire, c := claimFixture(t)
			input := dispatchInputForDeclaration()
			original, err := declareDispatch(t, b, c, input)
			require.NoError(t, err)
			_, ik, err := b.namespace.runtimeDispatchKeys(acquire.Workspace.Partition(), acquire.IntentID)
			require.NoError(t, err)
			_, rk, err := b.stageKeys(original.Reference)
			require.NoError(t, err)
			key := ik
			if part == "receipt" {
				key = rk
			}
			_, err = raw.Delete(context.Background(), key)
			require.NoError(t, err)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := declareDispatch(t, b, c, input)
			require.Error(t, err)
			require.Nil(t, result.Entry)
			require.Zero(t, lease.grants.Load())
			require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
		})
	}
}

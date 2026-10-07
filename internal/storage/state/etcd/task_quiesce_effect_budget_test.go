package etcd

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

// This unsealed tuple exercises only the pure mutation builder, not a TaskClaim
// capability, actual server revisions, Stage or metadata authority.
func quiescenceBuilderFixture(t *testing.T) (*Backend, *TaskClaim, *taskQuiescenceDraft) {
	r := quiesceRecordFixture(t)
	n := namespaceFromTaskCloseTest(t, r.Task.Reference)
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	key := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
	now := time.Now().UTC()
	ctx := r.Context.Current
	cert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: ctx.Namespace, AuthorityID: ctx.AuthorityID, Target: ctx.Target, RestoreEpoch: ctx.RestoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	v, err := p.NewManagementVerifier(p.TrustBinding{Namespace: ctx.Namespace, AuthorityID: ctx.AuthorityID, Target: ctx.Target, RestoreEpoch: ctx.RestoreEpoch}, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	issuer, err := v.VerifyCommandIssuerCertificate(cert, now)
	require.NoError(t, err)
	r.Context.Current.IssuerCertificateID = issuer.CertificateID()
	r.Context.Current.IssuerCertificateDigest = issuer.Digest()
	claims := p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: r.Context, NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)}
	wire, err := p.SignTaskUserQuiescenceTicket(key, issuer, claims)
	require.NoError(t, err)
	ticket, err := v.VerifyTaskUserQuiescenceTicket(wire, cert, r.Context, now)
	require.NoError(t, err)
	entry := &CommandIssuerEntry{Record: CommandIssuerRecord{Version: 1, Namespace: n.Root(), RestoreEpoch: r.Task.Reference.RestoreEpoch, CertificateID: issuer.CertificateID(), CertificateDigest: issuer.Digest(), Certificate: cert}, Revision: 2}
	b := &Backend{namespace: n}
	c := &TaskClaim{reference: r.Claim}
	for i := 0; i < 8; i++ {
		k, err := n.Key("model", fmt.Sprint(i))
		require.NoError(t, err)
		c.fences = append(c.fences, taskFence{key: k, value: "original", create: 1, mod: 1})
	}
	c.claimKey, _ = n.taskClaimKey(r.Task.Reference)
	c.guardKey, _ = n.taskGuardKey(r.Task.Reference, r.Claim.ClaimID)
	c.value = "claim"
	issuerKey, _ := n.commandIssuerKey(issuer.CertificateID())
	priorKey, _ := n.taskCloseDataKey(r.Task.Reference)
	receiptKey, _ := n.Key("model", "original-receipt")
	d := &taskQuiescenceDraft{task: r.Task, commandID: claims.Context.Current.CommandID, claims: claims, ticket: ticket, issuer: entry, issuerKey: issuerKey, issuerValue: "original issuer", prior: &taskQuiescencePrerequisite{intent: taskFence{key: priorKey, value: "original prior intent", create: r.Context.CloseDataIntentRevision, mod: r.Context.CloseDataIntentRevision}, receiptKey: receiptKey, receiptRevision: r.Context.CloseDataIntentRevision, proof: r.CloseDataReceipt}}
	return b, c, d
}
func TestTaskQuiesceMetadataBudget(t *testing.T) {
	b, c, d := quiescenceBuilderFixture(t)
	l := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: c.reference.Task.Partition, RestoreEpoch: c.reference.Task.RestoreEpoch, RequestID: c.reference.ClaimID, StageID: "task_quiesce_users", AttemptID: uuid.NewString()}
	m, err := b.buildTaskQuiescence(c, d, l)
	require.NoError(t, err)
	require.Len(t, m.Comparisons, 49)
	require.Len(t, m.Writes, 1)
	require.Equal(t, 64, len(m.Comparisons)+len(m.Writes)+stageProtocolOperations)
	require.Equal(t, c.comparisons(), m.Comparisons[:40])
	require.Equal(t, d.prior.intent.comparisons(), m.Comparisons[43:47])
	require.Equal(t, clientv3.Compare(clientv3.ModRevision(d.prior.receiptKey), "=", d.prior.receiptRevision), m.Comparisons[47])
	key, _ := b.namespace.taskQuiescenceKey(c.reference.Task)
	require.Equal(t, clientv3.Compare(clientv3.CreateRevision(key), "=", 0), m.Comparisons[48])
	require.Equal(t, key, m.Writes[0].Key)
	var w taskQuiescenceRecordWire
	require.NoError(t, json.Unmarshal(m.Writes[0].Value, &w))
	require.Equal(t, l, w.Attempt)
	require.Equal(t, []byte(d.prior.proof), []byte(w.CloseDataReceipt))
	for _, kind := range []string{"65", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			bad := Mutation{Comparisons: append([]clientv3.Cmp{}, m.Comparisons...), Writes: m.Writes}
			if kind == "65" {
				bad.Comparisons = append(bad.Comparisons, clientv3.Compare(clientv3.ModRevision(key), "=", 0))
			} else {
				for i := 0; i < 8; i++ {
					bad.Comparisons[i*4] = clientv3.Compare(clientv3.Value(c.fences[i].key), "=", strings.Repeat("x", maxRecordBytes))
				}
			}
			_, _, err := prepareMutation(b.namespace, bad)
			require.ErrorIs(t, err, ErrInvalidMutation)
		})
	}
	_, err = b.PrepareTaskQuiescence(context.Background(), c, nil)
	require.ErrorIs(t, err, ErrInvalidRecord)
}

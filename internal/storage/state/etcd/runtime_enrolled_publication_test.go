package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestEnrolledPublicationRecords(t *testing.T) {
	r := RuntimeEnrollmentRecord{Version: 2, IntentID: "11111111-1111-4111-8111-111111111111", SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), RestoreEpoch: "restore", Generation: 1, Runtime: RuntimeReference{ID: "pod", UID: "pod-uid", BootID: "boot"}, ObserverCertificate: json.RawMessage(`{"claims":{},"signature":"x"}`), Receipt: json.RawMessage(`{"claims":{},"signature":"x"}`)}
	wire, err := encodeEnrolledRecord(r)
	require.NoError(t, err)
	var got RuntimeEnrollmentRecord
	require.NoError(t, decodeEnrolledRecord([]byte(wire), &got))
	require.Equal(t, r, got)
	for _, bad := range []string{" " + wire, strings.Replace(wire, `"version":2`, `"version":2,"version":2`, 1), strings.Replace(wire, `"receipt":`, `"unknown":null,"receipt":`, 1), strings.Replace(wire, `"receipt":{"claims":{},"signature":"x"}`, `"receipt":null`, 1)} {
		require.Error(t, decodeEnrolledRecord([]byte(bad), &got))
	}
	r.Receipt = json.RawMessage(`{"large":"` + strings.Repeat("x", 8192) + `"}`)
	_, err = encodeEnrolledRecord(r)
	require.Error(t, err)
	a := RuntimeEnrollmentActivationRecord{Version: 2, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, Runtime: r.Runtime, Activation: json.RawMessage(`{}`), RuntimeCertificate: json.RawMessage(`{}`), IssuerCertificate: json.RawMessage(`{}`)}
	_, err = encodeEnrolledRecord(a)
	require.NoError(t, err)
	a.Activation = json.RawMessage(`{"large":"` + strings.Repeat("x", 8192) + `"}`)
	_, err = encodeEnrolledRecord(a)
	require.Error(t, err)
}

func TestEnrolledPublicationBounds(t *testing.T) {
	o := validOptions(t)
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey)}}
	o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: time.Now().UTC()}, nil })
	for _, count := range []uint32{0, 33} {
		o.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: strings.Repeat("a", 64), MaxActive: count}
		require.Error(t, validateOptions(o))
	}
	o.EnrolledPublication.MaxActive = 1
	require.NoError(t, validateOptions(o))
	o.EnrolledPublication.MaxActive = 32
	require.NoError(t, validateOptions(o))
	n := o.Namespace
	b := &Backend{namespace: n, restoreEpoch: o.Identity.RestoreEpoch, identityValue: "identity"}
	b.identityKey, _ = n.Key("meta", "identity")
	b.restoreKey, _ = n.Key("meta", "restore_epoch")
	l := StageAttemptLocator{Namespace: n.Root(), Partition: 0, RequestID: "request", StageID: enrolledPublicationStage, AttemptID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: b.restoreEpoch}
	m := Mutation{}
	for i := 0; i < 46; i++ {
		m.Comparisons = append(m.Comparisons, clientv3.Compare(clientv3.CreateRevision(n.Root()+fmt.Sprintf("comparison-%d", i)), "=", int64(i)))
	}
	for i := 0; i < 8; i++ {
		m.Writes = append(m.Writes, Write{Key: n.Root() + fmt.Sprintf("write-%d", i), Value: []byte("value")})
	}
	owned, digest, err := prepareEnrolledMutation(n, m)
	require.NoError(t, err)
	require.Len(t, digest, 64)
	ref := l.reference(digest)
	require.NoError(t, b.preflightEnrolledStage(ref, owned, 1, 1))
	m.Comparisons = append(m.Comparisons, m.Comparisons[0])
	_, _, err = prepareEnrolledMutation(n, m)
	require.Error(t, err)
	m = owned
	m.Writes[0].Value = bytes.Repeat([]byte("x"), 262144)
	_, _, err = prepareEnrolledMutation(n, m)
	require.Error(t, err)
}

func TestEnrolledPublicationMaximumNamespace(t *testing.T) {
	n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	require.Len(t, n.Root(), 512)
	b := &Backend{namespace: n, restoreEpoch: strings.Repeat("e", 128), identityValue: strings.Repeat("i", 2048)}
	b.identityKey, _ = n.Key("meta", "identity")
	b.restoreKey, _ = n.Key("meta", "restore_epoch")
	l := StageAttemptLocator{Namespace: n.Root(), RequestID: strings.Repeat("r", 128), StageID: enrolledPublicationStage, AttemptID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: b.restoreEpoch}
	m := Mutation{}
	for i := 0; i < 46; i++ {
		m.Comparisons = append(m.Comparisons, clientv3.Compare(clientv3.CreateRevision(n.Root()+fmt.Sprintf("cmp-%d", i)), "=", math.MaxInt64))
	}
	for i := 0; i < 8; i++ {
		m.Writes = append(m.Writes, Write{Key: n.Root() + fmt.Sprintf("write-%d", i), Value: bytes.Repeat([]byte(`\u003c`), 1000)})
	}
	owned, digest, err := prepareEnrolledMutation(n, m)
	require.NoError(t, err)
	require.NoError(t, b.preflightEnrolledMaximum(l.reference(digest), owned))
	m.Writes[0].Value[0] ^= 1
	require.NotEqual(t, m.Writes[0].Value, owned.Writes[0].Value)
}

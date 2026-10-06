package etcd

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"strings"
	"testing"
)

func TestRuntimePreparationCodec(t *testing.T) {
	// Missing closed-codec registration would prevent durable UID reservations.
	r := RuntimeIndexRecord{Version: 1, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: domainHash, RestoreEpoch: "restore", Generation: 1, Runtime: RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}}
	wire, err := encodeDomainRecord(r)
	require.NoError(t, err)
	var decoded RuntimeIndexRecord
	require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &decoded))
	require.Equal(t, r, decoded)
	for _, bad := range []string{`{"version":1,"version":1}`, `null`} {
		require.Error(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(bad)}, &decoded))
	}
	for _, change := range []func(*RuntimeIndexRecord){func(r *RuntimeIndexRecord) { r.Runtime.UID = "" }, func(r *RuntimeIndexRecord) { r.Generation = 0 }} {
		copy := r
		change(&copy)
		_, err = encodeDomainRecord(copy)
		require.Error(t, err)
	}
	payload := json.RawMessage(`{"opaque":"metadata"}`)
	digest, err := snapshotDigest(payload)
	require.NoError(t, err)
	cert := RuntimeBindingCertificateRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: 1, CertificateDigest: digest, Payload: payload}
	_, err = encodeDomainRecord(cert)
	require.NoError(t, err)
	cert.CertificateDigest = domainHash
	_, err = encodeDomainRecord(cert)
	require.Error(t, err)
}

func TestRuntimePreparationStrictMetadata(t *testing.T) {
	r := RuntimeIndexRecord{Version: 1, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: domainHash, RestoreEpoch: "restore", Generation: 1, Runtime: RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}}
	wire, err := encodeDomainRecord(r)
	require.NoError(t, err)
	for _, bad := range []string{
		strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(wire, `"uid":"uid"`, `"uid":"uid","uid":"uid"`, 1),
		strings.Replace(wire, `"boot_id":"boot"`, `"boot_id":null`, 1),
		strings.Replace(wire, `"boot_id":"boot"`, `"other":"boot"`, 1),
		wire + ` {}`,
	} {
		var got RuntimeIndexRecord
		require.ErrorIs(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(bad)}, &got), ErrCorruptRecord)
	}
}

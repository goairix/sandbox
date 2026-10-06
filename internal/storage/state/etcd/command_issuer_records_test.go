package etcd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func commandRecordFixture(t *testing.T) CommandIssuerRecord {
	t.Helper()
	// Opaque history deliberately lacks a cryptographic certificate schema.
	certificate := json.RawMessage(`{"historical":"expired","unknown":null}`)
	digest, err := snapshotDigest(certificate)
	require.NoError(t, err)
	return CommandIssuerRecord{Version: 1, Namespace: "/test/authority/cell/", RestoreEpoch: "epoch", CertificateID: "11234567-89ab-4cde-8012-3456789abcde", CertificateDigest: digest, Certificate: certificate}
}
func commandRecordKV(t *testing.T, r CommandIssuerRecord) *mvccpb.KeyValue {
	t.Helper()
	value, err := encodeCommandIssuerRecord(r)
	require.NoError(t, err)
	return &mvccpb.KeyValue{Key: []byte(r.Namespace + "command-issuers/" + r.CertificateID), Value: []byte(value), CreateRevision: 4, ModRevision: 4}
}
func TestCommandIssuerRecord(t *testing.T) {
	t.Run("round trip and ownership", func(t *testing.T) {
		r := commandRecordFixture(t)
		r.Certificate = json.RawMessage(`{"historical":"<expired>","unknown":null}`)
		r.CertificateDigest, _ = snapshotDigest(r.Certificate)
		require.NoError(t, r.Validate())
		kv := commandRecordKV(t, r)
		require.Contains(t, string(kv.Value), "<expired>")
		var got CommandIssuerRecord
		require.NoError(t, decodeCommandIssuerRecord(kv, &got))
		require.Equal(t, r, got)
		r.Certificate[0] = '!'
		require.Equal(t, byte('{'), got.Certificate[0])
		got.Certificate[0] = '!'
		var again CommandIssuerRecord
		require.NoError(t, decodeCommandIssuerRecord(kv, &again))
		require.Equal(t, byte('{'), again.Certificate[0])
	})
	for _, fault := range []string{"version", "namespace relative", "namespace suffix", "namespace dot", "namespace short", "namespace over512", "epoch", "epoch over128", "nil UUID", "upper UUID", "bad UUID", "digest upper", "digest short", "digest nonhex", "digest mismatch", "wire empty", "wire null", "wire invalid JSON", "wire UTF8", "wire over4096"} {
		t.Run(fault, func(t *testing.T) {
			r := commandRecordFixture(t)
			switch fault {
			case "version":
				r.Version = 2
			case "namespace relative":
				r.Namespace = "test/authority/cell/"
			case "namespace suffix":
				r.Namespace = "/test/authority/cell"
			case "namespace dot":
				r.Namespace = "/test/../cell/"
			case "namespace short":
				r.Namespace = "/authority/cell/"
			case "namespace over512":
				r.Namespace = "/" + strings.Repeat("a", 500) + "/authority/cell/"
			case "epoch":
				r.RestoreEpoch = "../epoch"
			case "epoch over128":
				r.RestoreEpoch = strings.Repeat("a", 129)
			case "nil UUID":
				r.CertificateID = "00000000-0000-0000-0000-000000000000"
			case "upper UUID":
				r.CertificateID = strings.ToUpper(r.CertificateID)
			case "bad UUID":
				r.CertificateID = "invalid"
			case "digest upper":
				r.CertificateDigest = strings.ToUpper(r.CertificateDigest)
			case "digest short":
				r.CertificateDigest = strings.Repeat("a", 63)
			case "digest nonhex":
				r.CertificateDigest = strings.Repeat("z", 64)
			case "digest mismatch":
				r.CertificateDigest = strings.Repeat("0", 64)
			case "wire empty":
				r.Certificate = nil
			case "wire null":
				r.Certificate = json.RawMessage("null")
			case "wire invalid JSON":
				r.Certificate = json.RawMessage("{")
			case "wire UTF8":
				r.Certificate = json.RawMessage{'"', 0xff, '"'}
			case "wire over4096":
				r.Certificate = json.RawMessage(`"` + strings.Repeat("a", 4095) + `"`)
			}
			if strings.HasPrefix(fault, "wire") {
				if d, err := snapshotDigest(r.Certificate); err == nil {
					r.CertificateDigest = d
				}
			}
			require.ErrorIs(t, r.Validate(), ErrInvalidRecord)
			_, err := encodeCommandIssuerRecord(r)
			require.ErrorIs(t, err, ErrInvalidRecord)
		})
	}
	t.Run("strict outer schema and atomic decode", func(t *testing.T) {
		r := commandRecordFixture(t)
		kv := commandRecordKV(t, r)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(kv.Value, &fields))
		for name := range fields {
			for _, fault := range []string{"missing", "null"} {
				t.Run(fault+" "+name, func(t *testing.T) {
					var object map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(kv.Value, &object))
					if fault == "missing" {
						delete(object, name)
					} else {
						object[name] = json.RawMessage("null")
					}
					wire, err := json.Marshal(object)
					require.NoError(t, err)
					corrupt := *kv
					corrupt.Value = wire
					got := r
					require.ErrorIs(t, decodeCommandIssuerRecord(&corrupt, &got), ErrCorruptRecord)
					require.Equal(t, r, got)
				})
			}
		}
		wire := string(kv.Value)
		for _, corruptWire := range []string{wire + ` {}`, wire[:len(wire)-1] + `,"other":true}`, wire[:len(wire)-1] + `,"version":1}`, "null", "[]", strings.Repeat(" ", 8193) + wire, string([]byte{'"', 0xff, '"'})} {
			corrupt := *kv
			corrupt.Value = []byte(corruptWire)
			got := r
			require.ErrorIs(t, decodeCommandIssuerRecord(&corrupt, &got), ErrCorruptRecord)
			require.Equal(t, r, got)
		}
	})
	t.Run("immutable envelope and exact key", func(t *testing.T) {
		for _, fault := range []string{"lease", "zero create", "negative create", "changed mod", "zero mod", "key UUID", "key namespace", "key suffix", "empty key"} {
			t.Run(fault, func(t *testing.T) {
				r := commandRecordFixture(t)
				kv := commandRecordKV(t, r)
				switch fault {
				case "lease":
					kv.Lease = 9
				case "zero create":
					kv.CreateRevision = 0
				case "negative create":
					kv.CreateRevision = -1
				case "changed mod":
					kv.ModRevision++
				case "zero mod":
					kv.ModRevision = 0
				case "key UUID":
					kv.Key = []byte(r.Namespace + "command-issuers/21234567-89ab-4cde-8012-3456789abcde")
				case "key namespace":
					kv.Key = []byte("/other/authority/cell/command-issuers/" + r.CertificateID)
				case "key suffix":
					kv.Key = append(kv.Key, '/')
				case "empty key":
					kv.Key = nil
				}
				got := r
				require.ErrorIs(t, decodeCommandIssuerRecord(kv, &got), ErrCorruptRecord)
				require.Equal(t, r, got)
			})
		}
		require.ErrorIs(t, decodeCommandIssuerRecord(nil, new(CommandIssuerRecord)), ErrCorruptRecord)
		require.ErrorIs(t, decodeCommandIssuerRecord(commandRecordKV(t, commandRecordFixture(t)), nil), ErrCorruptRecord)
	})
	t.Run("bounds", func(t *testing.T) {
		r := commandRecordFixture(t)
		r.Certificate = json.RawMessage(`"` + strings.Repeat("a", 4094) + `"`)
		r.CertificateDigest, _ = snapshotDigest(r.Certificate)
		wire, err := encodeCommandIssuerRecord(r)
		require.NoError(t, err)
		require.LessOrEqual(t, len(wire), 8192)
		kv := commandRecordKV(t, r)
		require.NoError(t, decodeCommandIssuerRecord(kv, new(CommandIssuerRecord)))
	})
	t.Run("point key", func(t *testing.T) {
		n := validOptions(t).Namespace
		key, err := n.commandIssuerKey("11234567-89ab-4cde-8012-3456789abcde")
		require.NoError(t, err)
		require.Equal(t, "/test/authority/cell/command-issuers/11234567-89ab-4cde-8012-3456789abcde", key)
		for _, id := range []string{"", "../issuer", "00000000-0000-0000-0000-000000000000", "11234567-89AB-4cde-8012-3456789abcde"} {
			_, err := n.commandIssuerKey(id)
			require.ErrorIs(t, err, ErrInvalidRecord)
		}
		_, err = (Namespace{}).commandIssuerKey("11234567-89ab-4cde-8012-3456789abcde")
		require.Error(t, err)
	})
}

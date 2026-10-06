package etcd

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func execEffectFixture() ExecEffectRecord {
	ticket := json.RawMessage(`{"expired":true,"wire":"<>&"}`)
	digest, _ := snapshotDigest(ticket)
	op := operationRecordFixture()
	return ExecEffectRecord{Version: 1, Operation: op, AdmissionRevision: 11, CommandID: "12345678-1234-4234-8234-123456789abc", DescriptorDigest: strings.Repeat("c", 64), TicketDigest: digest, IssuerCertificateID: "22345678-1234-4234-8234-123456789abc", IssuerCertificateDigest: strings.Repeat("d", 64), IssuerRevision: 9, Ticket: ticket, Attempt: StageAttemptLocator{Namespace: op.Reference.Namespace, Partition: op.Reference.Partition, RequestID: op.Reference.RequestID, RestoreEpoch: op.Reference.RestoreEpoch, StageID: "operation_exec_start", AttemptID: "32345678-1234-4234-8234-123456789abc"}}
}
func execEffectRef(r ExecEffectRecord) ExecEffectReference {
	return ExecEffectReference{Operation: r.Operation.Reference, CommandID: r.CommandID, Stage: r.Attempt.reference(strings.Repeat("e", 64))}
}
func execEffectKV(t *testing.T, r ExecEffectRecord) *mvccpb.KeyValue {
	t.Helper()
	var value bytes.Buffer
	enc := json.NewEncoder(&value)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(r))
	return &mvccpb.KeyValue{Key: []byte(r.Operation.Reference.Namespace + "p/aa/intents/" + r.Operation.Reference.OperationID + "/exec-start"), Value: bytes.TrimSuffix(value.Bytes(), []byte("\n")), CreateRevision: 15, ModRevision: 15}
}
func TestExecEffectRecordRoundTrip(t *testing.T) {
	r := execEffectFixture()
	require.NoError(t, r.Validate())
	wire, err := encodeExecEffectRecord(r)
	require.NoError(t, err)
	require.Contains(t, wire, `"wire":"<>&"`)
	kv := execEffectKV(t, r)
	require.Equal(t, string(kv.Value), wire)
	old := execEffectFixture()
	old.CommandID = "old"
	require.NoError(t, decodeExecEffectRecord(kv, &old))
	require.Equal(t, r, old)
	kv.Value[0] = '!'
	require.Equal(t, r, old)
	old.Ticket[0] = '!'
	require.Equal(t, byte('{'), r.Ticket[0])
	n, err := NewNamespace("/test", "scope", "cell")
	require.NoError(t, err)
	key, err := n.execEffectKey(r.Operation.Reference)
	require.NoError(t, err)
	require.Equal(t, "/test/scope/cell/p/aa/intents/01234567-89ab-4cde-8012-3456789abcde/exec-start", key)
	other, _ := NewNamespace("/other", "scope", "cell")
	_, err = other.execEffectKey(r.Operation.Reference)
	require.ErrorIs(t, err, ErrIdentityMismatch)
}
func TestExecEffectRecordValidation(t *testing.T) {
	cases := map[string]func(*ExecEffectRecord){
		"version": func(r *ExecEffectRecord) { r.Version = 2 }, "mutation": func(r *ExecEffectRecord) { r.Operation.Reference.Kind = OperationMutation }, "operation": func(r *ExecEffectRecord) { r.Operation.ControlRevision = 0 },
		"admission": func(r *ExecEffectRecord) { r.AdmissionRevision = 0 }, "issuer revision": func(r *ExecEffectRecord) { r.IssuerRevision = -1 },
		"command nil": func(r *ExecEffectRecord) { r.CommandID = "00000000-0000-0000-0000-000000000000" }, "command canonical": func(r *ExecEffectRecord) { r.CommandID = strings.ToUpper(r.CommandID) }, "issuer nil": func(r *ExecEffectRecord) { r.IssuerCertificateID = "00000000-0000-0000-0000-000000000000" },
		"descriptor": func(r *ExecEffectRecord) { r.DescriptorDigest = strings.Repeat("A", 64) }, "ticket digest": func(r *ExecEffectRecord) { r.TicketDigest = strings.Repeat("a", 64) }, "issuer digest": func(r *ExecEffectRecord) { r.IssuerCertificateDigest = "bad" },
		"ticket null": func(r *ExecEffectRecord) { r.Ticket = []byte(" null ") }, "ticket absent": func(r *ExecEffectRecord) { r.Ticket = nil }, "ticket utf8": func(r *ExecEffectRecord) { r.Ticket = []byte{'"', 0xff, '"'} }, "ticket invalid": func(r *ExecEffectRecord) { r.Ticket = []byte("{") }, "ticket large": func(r *ExecEffectRecord) {
			r.Ticket = []byte(`"` + strings.Repeat("x", 4095) + `"`)
			r.TicketDigest, _ = snapshotDigest(r.Ticket)
		},
		"attempt nil": func(r *ExecEffectRecord) { r.Attempt.AttemptID = "00000000-0000-0000-0000-000000000000" }, "attempt invalid": func(r *ExecEffectRecord) { r.Attempt.AttemptID = "bad" }, "stage": func(r *ExecEffectRecord) { r.Attempt.StageID = "other" }, "namespace": func(r *ExecEffectRecord) { r.Attempt.Namespace = "/other/scope/cell/" }, "partition": func(r *ExecEffectRecord) { r.Attempt.Partition++ }, "request": func(r *ExecEffectRecord) { r.Attempt.RequestID = "other" }, "restore": func(r *ExecEffectRecord) { r.Attempt.RestoreEpoch = "other" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := execEffectFixture()
			change(&r)
			require.ErrorIs(t, r.Validate(), ErrInvalidRecord)
			_, err := encodeExecEffectRecord(r)
			require.ErrorIs(t, err, ErrInvalidRecord)
			old := execEffectFixture()
			saved := old
			kv := execEffectKV(t, execEffectFixture())
			if json.Valid(r.Ticket) || len(r.Ticket) == 0 {
				kv = execEffectKV(t, r)
			} else {
				kv.Value = []byte(strings.Replace(string(kv.Value), string(execEffectFixture().Ticket), "{", 1))
			}
			require.ErrorIs(t, decodeExecEffectRecord(kv, &old), ErrCorruptRecord)
			require.Equal(t, saved, old)
		})
	}
	r := execEffectFixture()
	r.Ticket = []byte(`"` + strings.Repeat("x", 4094) + `"`)
	r.TicketDigest, _ = snapshotDigest(r.Ticket)
	require.NoError(t, r.Validate())
	_, err := encodeExecEffectRecord(r)
	require.NoError(t, err)
}
func TestExecEffectRecordStrictTypedFields(t *testing.T) {
	r := execEffectFixture()
	// Remove/null/duplicate/case/unknown every field of each typed object. Raw
	// ticket contents are deliberately opaque; this traversal never enters them.
	var visit func([]string, reflect.Type)
	visit = func(path []string, typ reflect.Type) {
		if typ == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := field.Tag.Get("json")
			p := append(append([]string(nil), path...), name)
			for _, defect := range []string{"missing", "null", "duplicate", "case", "unknown"} {
				t.Run(strings.Join(p, "/")+"/"+defect, func(t *testing.T) {
					kv := execEffectKV(t, r)
					var mutate func([]byte, []string) []byte
					mutate = func(wire []byte, parts []string) []byte {
						var object map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &object))
						key := parts[0]
						if len(parts) > 1 {
							object[key] = mutate(object[key], parts[1:])
							out, e := json.Marshal(object)
							require.NoError(t, e)
							return out
						}
						val := object[key]
						delete(object, key)
						out, e := json.Marshal(object)
						require.NoError(t, e)
						prefix := out[:len(out)-1]
						sep := ""
						if len(object) > 0 {
							sep = ","
						}
						switch defect {
						case "missing":
							return out
						case "null":
							return append(prefix, []byte(sep+`"`+key+`":null}`)...)
						case "duplicate":
							return append(prefix, []byte(sep+`"`+key+`":`+string(val)+`,"`+key+`":`+string(val)+`}`)...)
						case "case":
							return append(prefix, []byte(sep+`"`+strings.ToUpper(key)+`":`+string(val)+`}`)...)
						default:
							return append(prefix, []byte(sep+`"`+key+`":`+string(val)+`,"unexpected":1}`)...)
						}
					}
					kv.Value = mutate(kv.Value, p)
					old := r
					old.CommandID = "sentinel"
					saved := old
					require.ErrorIs(t, decodeExecEffectRecord(kv, &old), ErrCorruptRecord)
					require.Equal(t, saved, old)
				})
			}
			if field.Type.Kind() == reflect.Struct {
				visit(p, field.Type)
			}
		}
	}
	visit(nil, reflect.TypeOf(r))
}
func TestExecEffectRecordKVBoundaries(t *testing.T) {
	cases := map[string]func(*mvccpb.KeyValue){"key": func(k *mvccpb.KeyValue) { k.Key = []byte("other") }, "lease": func(k *mvccpb.KeyValue) { k.Lease = 1 }, "negative lease": func(k *mvccpb.KeyValue) { k.Lease = -1 }, "zero revision": func(k *mvccpb.KeyValue) { k.CreateRevision = 0; k.ModRevision = 0 }, "negative revision": func(k *mvccpb.KeyValue) { k.CreateRevision = -1; k.ModRevision = -1 }, "rewritten": func(k *mvccpb.KeyValue) { k.ModRevision++ }, "trailing": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, []byte(" {}")...) }, "utf8": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, 0xff) }, "oversize": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, []byte(strings.Repeat(" ", 16385))...) }, "null": func(k *mvccpb.KeyValue) { k.Value = []byte("null") }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := execEffectFixture()
			kv := execEffectKV(t, r)
			change(kv)
			old := r
			old.CommandID = "old"
			saved := old
			require.ErrorIs(t, decodeExecEffectRecord(kv, &old), ErrCorruptRecord)
			require.Equal(t, saved, old)
		})
	}
	kv := execEffectKV(t, execEffectFixture())
	kv.Value = append(kv.Value, bytes.Repeat([]byte(" "), 16384-len(kv.Value))...)
	var got ExecEffectRecord
	require.NoError(t, decodeExecEffectRecord(kv, &got))
	kv.Value = append(kv.Value, ' ')
	require.ErrorIs(t, decodeExecEffectRecord(kv, &got), ErrCorruptRecord)
	require.ErrorIs(t, decodeExecEffectRecord(nil, &got), ErrCorruptRecord)
	require.ErrorIs(t, decodeExecEffectRecord(kv, nil), ErrCorruptRecord)
}
func TestExecEffectReferenceValidation(t *testing.T) {
	ref := execEffectRef(execEffectFixture())
	require.NoError(t, ref.Validate())
	for name, change := range map[string]func(*ExecEffectReference){"operation": func(r *ExecEffectReference) { r.Operation.LeaseID = 0 }, "mutation": func(r *ExecEffectReference) { r.Operation.Kind = OperationMutation }, "command": func(r *ExecEffectReference) { r.CommandID = "bad" }, "nil command": func(r *ExecEffectReference) { r.CommandID = "00000000-0000-0000-0000-000000000000" }, "partial stage": func(r *ExecEffectReference) { r.Stage = StageReference{} }, "namespace": func(r *ExecEffectReference) { r.Stage.Namespace = "/other/scope/cell/" }, "partition": func(r *ExecEffectReference) { r.Stage.Partition++ }, "request": func(r *ExecEffectReference) { r.Stage.RequestID = "other" }, "restore": func(r *ExecEffectReference) { r.Stage.RestoreEpoch = "other" }, "stage": func(r *ExecEffectReference) { r.Stage.StageID = "other" }, "attempt": func(r *ExecEffectReference) { r.Stage.AttemptID = "bad" }, "nil attempt": func(r *ExecEffectReference) { r.Stage.AttemptID = "00000000-0000-0000-0000-000000000000" }, "digest": func(r *ExecEffectReference) { r.Stage.Digest = strings.Repeat("A", 64) }} {
		t.Run(name, func(t *testing.T) { bad := ref; change(&bad); require.ErrorIs(t, bad.Validate(), ErrInvalidRecord) })
	}
}
func TestExecEffectRecordLongestValidContext(t *testing.T) {
	r := execEffectFixture()
	op := &r.Operation
	op.Reference.Namespace = "/" + strings.Repeat("n", 253) + "/" + strings.Repeat("s", 128) + "/" + strings.Repeat("c", 127) + "/"
	require.Len(t, op.Reference.Namespace, 512)
	op.Reference.RestoreEpoch = strings.Repeat("r", 128)
	op.Reference.RequestID = strings.Repeat("q", 128)
	op.Reference.SandboxID = strings.Repeat("s", 128)
	op.Reference.Partition = 255
	op.WorkspaceHash = strings.Repeat("f", 64)
	op.IntentID = strings.Repeat("i", 128)
	op.Reference.LeaseID = math.MaxInt64
	op.Generation = math.MaxInt64
	op.DataGateEpoch = math.MaxInt64
	op.ControlRevision = math.MaxInt64
	op.Runtime = RuntimeReference{ID: strings.Repeat("<", 128), UID: strings.Repeat("<", 128), BootID: strings.Repeat("<", 128)}
	op.Snapshot.Version = strings.Repeat("v", 128)
	op.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	// HTML escaping in the original operation codec can reach its exact 4096B
	// boundary while the containing codec preserves the ticket's raw bytes.
	wire, _ := json.Marshal(*op)
	for len(wire) > 4096 {
		i := strings.Index(op.Runtime.ID, "<")
		if i < 0 {
			break
		}
		op.Runtime.ID = op.Runtime.ID[:i] + "x" + op.Runtime.ID[i+1:]
		wire, _ = json.Marshal(*op)
	}
	require.LessOrEqual(t, len(wire), 4096)
	// Use backslash (2B) substitutions to fill a 1..4 byte remainder exactly.
	remaining := 4096 - len(wire)
	for i := 0; i < remaining; i++ {
		idx := strings.Index(op.Runtime.ID, "x")
		require.GreaterOrEqual(t, idx, 0)
		op.Runtime.ID = op.Runtime.ID[:idx] + "\\" + op.Runtime.ID[idx+1:]
	}
	operationWire, err := encodeOperationRecord(*op)
	require.NoError(t, err)
	require.Len(t, operationWire, 4096)
	r.Attempt.Namespace = op.Reference.Namespace
	r.Attempt.Partition = 255
	r.Attempt.RestoreEpoch = op.Reference.RestoreEpoch
	r.Attempt.RequestID = op.Reference.RequestID
	r.AdmissionRevision = math.MaxInt64
	r.IssuerRevision = math.MaxInt64
	r.Ticket = []byte(`"` + strings.Repeat("t", 4094) + `"`)
	r.TicketDigest, _ = snapshotDigest(r.Ticket)
	wireOut, err := encodeExecEffectRecord(r)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wireOut), 16384)
	t.Logf("longest context operation=%d ticket=%d encoded effect=%d", len(operationWire), len(r.Ticket), len(wireOut))
	// Every opaque runtime byte can cost at most two bytes without HTML
	// escaping (quotes/backslashes, or U+2028/U+2029 at 6 encoded / 3 UTF8).
	// All other typed fields are already at their permitted maxima above.
	op.Runtime = RuntimeReference{ID: strings.Repeat("\\", 128), UID: strings.Repeat("\\", 128), BootID: strings.Repeat("\\", 128)}
	operationWire, err = encodeOperationRecord(*op)
	require.NoError(t, err)
	wireOut, err = encodeExecEffectRecord(r)
	require.NoError(t, err)
	t.Logf("worst valid context operation=%d ticket=%d encoded effect=%d (limit16384)", len(operationWire), len(r.Ticket), len(wireOut))
	require.NoError(t, r.Validate())
	op.Runtime = RuntimeReference{ID: strings.Repeat("<", 128), UID: strings.Repeat("<", 128), BootID: strings.Repeat("<", 128)}
	require.NoError(t, op.Validate())
	require.ErrorIs(t, r.Validate(), ErrInvalidRecord)
}

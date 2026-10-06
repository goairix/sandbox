package etcd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

const dispatchUUID = "01234567-89ab-4cde-8012-3456789abcde"

func dispatchRecord() RuntimeDispatchRecord {
	return RuntimeDispatchRecord{Version: 1, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: domainHash, RequestHash: domainHash, ConfigurationDigest: domainHash, RestoreEpoch: "restore", Generation: 1, DataGateEpoch: 1, Snapshot: SnapshotReference{Version: "v1", Digest: domainHash}, ExpiresAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), OperationID: dispatchUUID, Kind: RuntimeDispatchCreate, Target: "runtime/编号", PayloadDigest: domainHash, Claim: DispatchClaimReference{ClaimID: dispatchUUID, WorkerID: "worker", CreateRevision: 1, LeaseID: 1}, Attempt: StageAttemptLocator{Namespace: "/state/scope/cell/", Partition: 0xaa, RequestID: "request", StageID: "runtime_dispatch", AttemptID: dispatchUUID, RestoreEpoch: "restore"}}
}
func dispatchInput() RuntimeDispatchInputRecord {
	return RuntimeDispatchInputRecord{Version: 1, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: domainHash, RestoreEpoch: "restore", OperationID: dispatchUUID, PayloadDigest: "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a", Generation: 1, Payload: json.RawMessage(`{}`)}
}
func TestRuntimeDispatchRecordValidation(t *testing.T) {
	for _, kind := range []RuntimeDispatchKind{RuntimeDispatchCreate, RuntimeDispatchPrepare} {
		r := dispatchRecord()
		r.Kind = kind
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for name, mutate := range map[string]func(*RuntimeDispatchRecord){
		"schema":              func(r *RuntimeDispatchRecord) { r.Version = 2 },
		"intent":              func(r *RuntimeDispatchRecord) { r.IntentID = "../x" },
		"sandbox":             func(r *RuntimeDispatchRecord) { r.SandboxID = "" },
		"workspace":           func(r *RuntimeDispatchRecord) { r.WorkspaceHash = "bad" },
		"request":             func(r *RuntimeDispatchRecord) { r.RequestHash = "bad" },
		"config":              func(r *RuntimeDispatchRecord) { r.ConfigurationDigest = "bad" },
		"restore":             func(r *RuntimeDispatchRecord) { r.RestoreEpoch = "" },
		"generation":          func(r *RuntimeDispatchRecord) { r.Generation = 0 },
		"gate":                func(r *RuntimeDispatchRecord) { r.DataGateEpoch = 0 },
		"snapshot":            func(r *RuntimeDispatchRecord) { r.Snapshot.Digest = "bad" },
		"expiry":              func(r *RuntimeDispatchRecord) { r.ExpiresAt = time.Time{} },
		"expiry zone":         func(r *RuntimeDispatchRecord) { r.ExpiresAt = r.ExpiresAt.In(time.FixedZone("zone", 3600)) },
		"operation canonical": func(r *RuntimeDispatchRecord) { r.OperationID = strings.ToUpper(dispatchUUID) },
		"kind":                func(r *RuntimeDispatchRecord) { r.Kind = "delete" },
		"target control":      func(r *RuntimeDispatchRecord) { r.Target = "a\u0085" },
		"target UTF8":         func(r *RuntimeDispatchRecord) { r.Target = "\xff" },
		"target bound":        func(r *RuntimeDispatchRecord) { r.Target = strings.Repeat("x", 129) },
		"digest":              func(r *RuntimeDispatchRecord) { r.PayloadDigest = strings.Repeat("A", 64) },
		"claim id":            func(r *RuntimeDispatchRecord) { r.Claim.ClaimID = "bad" },
		"worker":              func(r *RuntimeDispatchRecord) { r.Claim.WorkerID = strings.Repeat("x", 129) },
		"claim revision":      func(r *RuntimeDispatchRecord) { r.Claim.CreateRevision = 0 },
		"claim lease":         func(r *RuntimeDispatchRecord) { r.Claim.LeaseID = 0 },
		"locator":             func(r *RuntimeDispatchRecord) { r.Attempt.AttemptID = "bad" },
		"partition":           func(r *RuntimeDispatchRecord) { r.Attempt.Partition = 1 },
		"attempt restore":     func(r *RuntimeDispatchRecord) { r.Attempt.RestoreEpoch = "different" },
		"attempt stage":       func(r *RuntimeDispatchRecord) { r.Attempt.StageID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			r := dispatchRecord()
			mutate(&r)
			if !errors.Is(r.Validate(), ErrInvalidRecord) {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
}
func TestRuntimeDispatchInputValidation(t *testing.T) {
	if err := dispatchInput().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RuntimeDispatchInputRecord){"schema": func(r *RuntimeDispatchInputRecord) { r.Version = 2 },
		"ownership": func(r *RuntimeDispatchInputRecord) { r.SandboxID = "" },
		"operation": func(r *RuntimeDispatchInputRecord) { r.OperationID = "bad" },
		"digest":    func(r *RuntimeDispatchInputRecord) { r.PayloadDigest = domainHash },
		"nil":       func(r *RuntimeDispatchInputRecord) { r.Payload = nil },
		"trailing":  func(r *RuntimeDispatchInputRecord) { r.Payload = []byte(`{} {}`) },
		"utf8":      func(r *RuntimeDispatchInputRecord) { r.Payload = []byte("\"\xff\"") }} {
		t.Run(name, func(t *testing.T) {
			r := dispatchInput()
			mutate(&r)
			if !errors.Is(r.Validate(), ErrInvalidRecord) {
				t.Fatal("invalid input accepted")
			}
		})
	}
}
func TestRuntimeDispatchCodecAndKeys(t *testing.T) {
	n, _ := NewNamespace("/state", "scope", "cell")
	declaration, input, err := n.runtimeDispatchKeys(0xaa, "intent")
	if err != nil || declaration != "/state/scope/cell/p/aa/intents/intent/runtime-dispatch" || input != "/state/scope/cell/p/aa/intents/intent/runtime-input" {
		t.Fatalf("fixed keys: %q %q %v", declaration, input, err)
	}
	if _, _, err := n.runtimeDispatchKeys(0, "../x"); err == nil {
		t.Fatal("invalid intent accepted")
	}
	for _, r := range []interface{ Validate() error }{dispatchRecord(), dispatchInput()} {
		if _, err := encodeDomainRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []interface{ Validate() error }{(*RuntimeDispatchRecord)(nil), (*RuntimeDispatchInputRecord)(nil)} {
		if _, err := encodeDomainRecord(r); !errors.Is(err, ErrInvalidRecord) {
			t.Fatal("nil encode accepted")
		}
		if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(`{}`)}, r); !errors.Is(err, ErrCorruptRecord) {
			t.Fatal("nil decode accepted")
		}
	}
	r := dispatchRecord()
	wire, err := encodeDomainRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	var out RuntimeDispatchRecord
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &out); err != nil || out != r {
		t.Fatalf("roundtrip: %+v %v", out, err)
	}
	for name, value := range map[string]string{"unknown": strings.TrimSuffix(wire, "}") + `,"extra":1}`, "trailing": wire + ` {}`, "utf8": "\xff", "oversize": wire + strings.Repeat(" ", 4096), "missing": strings.Replace(wire, `"generation":1,`, "", 1)} {
		t.Run(name, func(t *testing.T) {
			before := out
			if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(value)}, &out); !errors.Is(err, ErrCorruptRecord) {
				t.Fatal("bad wire accepted")
			}
			if out != before {
				t.Fatal("failed decode changed destination")
			}
		})
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire), Lease: 1}, &out); !errors.Is(err, ErrCorruptRecord) {
		t.Fatal("leased declaration accepted")
	}
}
func TestRuntimeDispatchPayloadRepresentationAndWireBudget(t *testing.T) {
	r := dispatchInput()
	r.Payload = json.RawMessage(" { \"z\":9007199254740993, \"a\":1e+03, \"text\":\"<>&\u2028\u2029\" } ")
	r.PayloadDigest = "1d58e2d63018beb2c8055cd76b5fd5cca9ca8192e0bc681a0d257d7d396dede2"
	wire, err := encodeDomainRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Payload[1] = 'x'
	var out RuntimeDispatchInputRecord
	kv := &mvccpb.KeyValue{Value: []byte(wire)}
	if err := decodeDomainRecord(kv, &out); err != nil {
		t.Fatal(err)
	}
	want := "{\"z\":9007199254740993,\"a\":1e+03,\"text\":\"<>&\u2028\u2029\"}"
	if string(out.Payload) != want {
		t.Fatalf("payload representation changed: %s", out.Payload)
	}
	for i := range kv.Value {
		kv.Value[i] = 'x'
	}
	if string(out.Payload) != want {
		t.Fatal("decoded payload aliases storage")
	}
	base := dispatchInput()
	base.Payload = []byte(`""`)
	base.PayloadDigest, _ = snapshotDigest(base.Payload)
	small, err := encodeDomainRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Payload = []byte(`"` + strings.Repeat("x", 65536-len(small)) + `"`)
	base.PayloadDigest, _ = snapshotDigest(base.Payload)
	exact, err := encodeDomainRecord(base)
	if err != nil || len(exact) != 65536 {
		t.Fatalf("exact budget: bytes=%d err=%v", len(exact), err)
	}
	base.Payload = []byte(`"` + strings.Repeat("x", 65537-len(small)) + `"`)
	base.PayloadDigest, _ = snapshotDigest(base.Payload)
	if _, err := encodeDomainRecord(base); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal("full input wire budget ignored")
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(exact + " ")}, &out); !errors.Is(err, ErrCorruptRecord) {
		t.Fatal("oversize input accepted")
	}
}

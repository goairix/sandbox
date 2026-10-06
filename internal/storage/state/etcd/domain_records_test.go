package etcd

import (
	"encoding/json"
	"errors"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"strings"
	"testing"
	"time"
)

const domainHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func domainOwner() WorkspaceOwnerRecord {
	return WorkspaceOwnerRecord{Version: 1, WorkspaceHash: domainHash, SandboxID: "sandbox", IntentID: "intent", RestoreEpoch: "restore", Generation: 1}
}
func domainControl() SandboxControlRecord {
	return SandboxControlRecord{Version: 1, SandboxID: "sandbox", WorkspaceHash: domainHash, IntentID: "intent", RestoreEpoch: "restore", Generation: 1, DataGateEpoch: 1, Phase: PhasePublishing, Snapshot: SnapshotReference{Version: "v1", Digest: domainHash}, ExpiresAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
}
func domainRuntime() *RuntimeReference {
	return &RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}
}
func TestDomainOwnerValidation(t *testing.T) {
	o := domainOwner()
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	o.Runtime = domainRuntime()
	o.MountAttempt = 1
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*WorkspaceOwnerRecord){"schema": func(o *WorkspaceOwnerRecord) { o.Version = 2 }, "hash": func(o *WorkspaceOwnerRecord) { o.WorkspaceHash = strings.Repeat("A", 64) }, "sandbox": func(o *WorkspaceOwnerRecord) { o.SandboxID = "../x" }, "intent": func(o *WorkspaceOwnerRecord) { o.IntentID = "" }, "restore": func(o *WorkspaceOwnerRecord) { o.RestoreEpoch = strings.Repeat("x", 129) }, "generation": func(o *WorkspaceOwnerRecord) { o.Generation = 0 }, "provisional mount": func(o *WorkspaceOwnerRecord) { o.MountAttempt = 1 }, "runtime": func(o *WorkspaceOwnerRecord) { o.Runtime = &RuntimeReference{ID: "runtime", UID: "uid"} }, "mount": func(o *WorkspaceOwnerRecord) { o.Runtime = domainRuntime(); o.MountAttempt = 2 }} {
		t.Run(name, func(t *testing.T) {
			o := domainOwner()
			mutate(&o)
			if !errors.Is(o.Validate(), ErrInvalidRecord) {
				t.Error("invalid owner accepted")
			}
		})
	}
}
func TestDomainControlValidation(t *testing.T) {
	for _, phase := range []SandboxPhase{PhasePublishing, PhaseActive, PhaseWorkspaceExclusive, PhaseDestroying, PhaseCleanupPending} {
		c := domainControl()
		c.Phase = phase
		if phase != PhasePublishing {
			c.Runtime = domainRuntime()
			c.MountAttempt = 1
		}
		if err := c.Validate(); err != nil {
			t.Errorf("valid phase %s: %v", phase, err)
		}
	}
	for name, mutate := range map[string]func(*SandboxControlRecord){"schema": func(c *SandboxControlRecord) { c.Version = 0 }, "phase": func(c *SandboxControlRecord) { c.Phase = "unknown" }, "active runtime": func(c *SandboxControlRecord) { c.Phase = PhaseActive }, "publishing runtime": func(c *SandboxControlRecord) { c.Runtime = domainRuntime() }, "publishing mount": func(c *SandboxControlRecord) { c.MountAttempt = 1 }, "data gate": func(c *SandboxControlRecord) { c.DataGateEpoch = 0 }, "generation": func(c *SandboxControlRecord) { c.Generation = -1 }, "expiry": func(c *SandboxControlRecord) { c.ExpiresAt = time.Time{} }, "expiry year": func(c *SandboxControlRecord) { c.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, "hash": func(c *SandboxControlRecord) { c.WorkspaceHash = "bad" }, "snapshot": func(c *SandboxControlRecord) { c.Snapshot.Version = "../v" }, "mount": func(c *SandboxControlRecord) { c.Phase = PhaseActive; c.Runtime = domainRuntime(); c.MountAttempt = 2 }} {
		t.Run(name, func(t *testing.T) {
			c := domainControl()
			mutate(&c)
			if !errors.Is(c.Validate(), ErrInvalidRecord) {
				t.Error("invalid control accepted")
			}
		})
	}
}
func TestDomainReferencesAndOtherRecords(t *testing.T) {
	for _, r := range []RuntimeReference{{ID: "", UID: "u", BootID: "b"}, {ID: "x", UID: "\xff", BootID: "b"}, {ID: "x", UID: "u", BootID: "\u0085"}, {ID: strings.Repeat("x", 129), UID: "u", BootID: "b"}} {
		if !errors.Is(r.Validate(), ErrInvalidRecord) {
			t.Error("invalid runtime accepted")
		}
	}
	if err := (RuntimeReference{ID: "runtime/opaque", UID: "编号", BootID: "boot:1"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []SnapshotReference{{Version: "../v", Digest: domainHash}, {Version: "v", Digest: strings.Repeat("A", 64)}, {Version: "v", Digest: strings.Repeat("z", 64)}} {
		if !errors.Is(s.Validate(), ErrInvalidRecord) {
			t.Error("invalid snapshot reference accepted")
		}
	}
	f := WorkspaceFenceRecord{Version: 1, WorkspaceHash: domainHash, RestoreEpoch: "restore", Generation: 1}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	f.Generation = 0
	if !errors.Is(f.Validate(), ErrInvalidRecord) {
		t.Error("invalid fence accepted")
	}
	f.Generation = 1
	f.Version = 0
	if !errors.Is(f.Validate(), ErrInvalidRecord) {
		t.Error("fence schema accepted")
	}
	i := CreationIntentRecord{Version: 1, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: domainHash, RequestHash: domainHash, ConfigurationDigest: domainHash, RestoreEpoch: "restore", Generation: 1, Phase: "pending"}
	for _, p := range []string{"pending", "published"} {
		i.Phase = p
		if err := i.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	i.Phase = "completed"
	if !errors.Is(i.Validate(), ErrInvalidRecord) {
		t.Error("intent phase accepted")
	}
	i.Phase = "pending"
	i.RequestHash = "raw key"
	if !errors.Is(i.Validate(), ErrInvalidRecord) {
		t.Error("intent request hash accepted")
	}
	r := CreationRequestRecord{Version: 1, RequestID: "request", RequestHash: domainHash, ConfigurationDigest: domainHash, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: domainHash, RestoreEpoch: "restore", Generation: 1, Phase: "pending"}
	for _, p := range []string{"pending", "completed"} {
		r.Phase = p
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	r.Phase = "published"
	if !errors.Is(r.Validate(), ErrInvalidRecord) {
		t.Error("request phase accepted")
	}
	r.Phase = "pending"
	r.RequestID = "../escape"
	if !errors.Is(r.Validate(), ErrInvalidRecord) {
		t.Error("request id accepted")
	}
}
func TestDomainSnapshotDigestPreservesRepresentation(t *testing.T) {
	raw := json.RawMessage(` { "z": 9007199254740993, "a": 1e+03, "text":"<>&" } `)
	digest, err := snapshotDigest(raw)
	if err != nil || len(digest) != 64 {
		t.Fatalf("digest %q %v", digest, err)
	}
	s := SandboxSnapshotRecord{Version: 1, SandboxID: "sandbox", Snapshot: SnapshotReference{Version: "v1", Digest: digest}, Payload: raw}
	encoded, err := encodeDomainRecord(s)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SandboxSnapshotRecord
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded)}, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Payload) != `{"z":9007199254740993,"a":1e+03,"text":"<>&"}` {
		t.Fatalf("representation changed: %s", decoded.Payload)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	s.Snapshot.Digest = domainHash
	if !errors.Is(s.Validate(), ErrInvalidRecord) {
		t.Error("mismatched snapshot digest accepted")
	}
	for _, raw := range []json.RawMessage{nil, []byte(`{`), []byte("\"\xff\""), []byte(`{} {}`)} {
		if _, err := snapshotDigest(raw); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("invalid payload accepted: %v", err)
		}
	}
}
func TestDomainCodecStrictJSONAndLease(t *testing.T) {
	encoded, err := encodeDomainRecord(domainOwner())
	if err != nil || len(encoded) == 0 || len(encoded) >= 1024 {
		t.Fatalf("encode %q %v", encoded, err)
	}
	if !strings.Contains(encoded, `"workspace_hash"`) {
		t.Error("snake case field missing")
	}
	var out WorkspaceOwnerRecord
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded)}, &out); err != nil || out.SandboxID != "sandbox" {
		t.Fatalf("roundtrip: %+v %v", out, err)
	}
	for name, value := range map[string]string{"unknown": strings.TrimSuffix(encoded, "}") + `,"extra":true}`, "trailing": encoded + ` {}`, "schema": strings.Replace(encoded, `"version":1`, `"version":2`, 1), "generation": strings.Replace(encoded, `"generation":1`, `"generation":0`, 1), "null": "null", "malformed": "{", "oversize": encoded + strings.Repeat(" ", 4096), "utf8": "{\"x\":\"\xff\"}"} {
		t.Run(name, func(t *testing.T) {
			if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(value)}, &WorkspaceOwnerRecord{}); !errors.Is(err, ErrCorruptRecord) {
				t.Errorf("invalid storage accepted: %v", err)
			}
		})
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded), Lease: 1}, &out); !errors.Is(err, ErrCorruptRecord) {
		t.Error("lease accepted")
	}
	if err := decodeDomainRecord(nil, &out); !errors.Is(err, ErrCorruptRecord) {
		t.Error("nil KV accepted")
	}
	o := domainOwner()
	o.Version = 2
	if _, err := encodeDomainRecord(o); !errors.Is(err, ErrInvalidRecord) {
		t.Error("invalid input accepted")
	}
	if _, err := encodeDomainRecord((*WorkspaceOwnerRecord)(nil)); !errors.Is(err, ErrInvalidRecord) {
		t.Error("nil input accepted")
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded)}, (*WorkspaceOwnerRecord)(nil)); !errors.Is(err, ErrCorruptRecord) {
		t.Error("nil destination accepted")
	}
}
func TestDomainCodecSizeLimits(t *testing.T) {
	f := WorkspaceFenceRecord{Version: 1, WorkspaceHash: domainHash, RestoreEpoch: "restore", Generation: 1}
	encoded, err := encodeDomainRecord(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded + strings.Repeat(" ", 1024))}, &WorkspaceFenceRecord{}); !errors.Is(err, ErrCorruptRecord) {
		t.Error("oversize fence accepted")
	}
	raw := json.RawMessage(`{"data":"` + strings.Repeat("x", 65536) + `"}`)
	digest, _ := snapshotDigest(raw)
	s := SandboxSnapshotRecord{Version: 1, SandboxID: "sandbox", Snapshot: SnapshotReference{Version: "v", Digest: digest}, Payload: raw}
	if _, err := encodeDomainRecord(s); !errors.Is(err, ErrInvalidRecord) {
		t.Error("oversize snapshot accepted")
	}
	raw = json.RawMessage(`{"data":"` + strings.Repeat("x", 60000) + `"}`)
	digest, _ = snapshotDigest(raw)
	s.Payload = raw
	s.Snapshot.Digest = digest
	encoded, err = encodeDomainRecord(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded)}, &SandboxSnapshotRecord{}); err != nil {
		t.Fatal(err)
	}
}
func TestDomainPlacementValidation(t *testing.T) {
	valid := SandboxPlacementRecord{Version: 1, SandboxID: "sandbox", WorkspaceHash: domainHash, IntentID: "intent", RestoreEpoch: "restore", Partition: 0xaa, Generation: 1}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*SandboxPlacementRecord){"partition": func(r *SandboxPlacementRecord) { r.Partition = 0 }, "schema": func(r *SandboxPlacementRecord) { r.Version = 0 }, "generation": func(r *SandboxPlacementRecord) { r.Generation = 0 }, "sandbox": func(r *SandboxPlacementRecord) { r.SandboxID = "../x" }, "hash": func(r *SandboxPlacementRecord) { r.WorkspaceHash = "bad" }, "restore": func(r *SandboxPlacementRecord) { r.RestoreEpoch = "" }, "intent": func(r *SandboxPlacementRecord) { r.IntentID = "" }} {
		t.Run(name, func(t *testing.T) {
			r := valid
			mutate(&r)
			if !errors.Is(r.Validate(), ErrInvalidRecord) {
				t.Error("invalid placement accepted")
			}
		})
	}
	encoded, err := encodeDomainRecord(valid)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SandboxPlacementRecord
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded)}, &decoded); err != nil || decoded != valid {
		t.Fatalf("placement roundtrip %+v %v", decoded, err)
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded + strings.Repeat(" ", 4096))}, &decoded); !errors.Is(err, ErrCorruptRecord) {
		t.Error("oversize placement accepted")
	}
}
func TestDomainSnapshotInputSizeAndUnicode(t *testing.T) {
	if _, err := snapshotDigest(json.RawMessage(` {}` + strings.Repeat(" ", 65536))); !errors.Is(err, ErrInvalidRecord) {
		t.Error("oversize raw JSON accepted")
	}
	raw := json.RawMessage("{\"text\":\"<>&\u2028\u2029\",\"number\":9007199254740993}")
	digest, err := snapshotDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := SandboxSnapshotRecord{Version: 1, SandboxID: "sandbox", Snapshot: SnapshotReference{Version: "v1", Digest: digest}, Payload: raw}
	encoded, err := encodeDomainRecord(s)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SandboxSnapshotRecord
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(encoded)}, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Payload) != string(raw) {
		t.Fatalf("snapshot bytes changed: %s", decoded.Payload)
	}
}
func TestDomainControlExpiryMustBeRepresentableUTC(t *testing.T) {
	for name, expiry := range map[string]time.Time{"year zero": time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), "non UTC": time.Date(2026, 10, 6, 0, 0, 0, 0, time.FixedZone("local", 3600))} {
		t.Run(name, func(t *testing.T) {
			c := domainControl()
			c.ExpiresAt = expiry
			if !errors.Is(c.Validate(), ErrInvalidRecord) {
				t.Error("invalid expiry accepted")
			}
		})
	}
	c := domainControl()
	c.ExpiresAt = time.Date(2026, 10, 6, 0, 0, 0, 0, time.FixedZone("zero", 0))
	if err := c.Validate(); err != nil {
		t.Fatalf("zero-offset expiry rejected: %v", err)
	}
}

func TestDomainRuntimeWireOmissionAndBinding(t *testing.T) {
	owner, control := domainOwner(), domainControl()
	for name, record := range map[string]interface{ Validate() error }{"owner provisional": owner, "control publishing": control} {
		t.Run(name, func(t *testing.T) {
			wire, err := encodeDomainRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(wire), &object); err != nil {
				t.Fatal(err)
			}
			if _, present := object["runtime"]; present {
				t.Fatal("nil runtime must be omitted")
			}
		})
	}
	owner.Runtime = domainRuntime()
	control.Phase = PhaseActive
	control.Runtime = domainRuntime()
	for name, record := range map[string]interface{ Validate() error }{"owner bound": owner, "control active": control} {
		t.Run(name, func(t *testing.T) {
			wire, err := encodeDomainRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(wire), &object); err != nil {
				t.Fatal(err)
			}
			var reference RuntimeReference
			if err := json.Unmarshal(object["runtime"], &reference); err != nil || reference != *domainRuntime() {
				t.Fatalf("bound runtime omitted or changed: %+v %v", reference, err)
			}
		})
	}
}
func TestDomainCleanupAllowsUnboundIntent(t *testing.T) {
	for _, phase := range []SandboxPhase{PhaseDestroying, PhaseCleanupPending} {
		t.Run(string(phase), func(t *testing.T) {
			control := domainControl()
			control.Phase = phase
			if err := control.Validate(); err != nil {
				t.Fatalf("unbound cleanup intent rejected: %v", err)
			}
			control.MountAttempt = 1
			if !errors.Is(control.Validate(), ErrInvalidRecord) {
				t.Fatal("unbound cleanup mount accepted")
			}
			control.Runtime = domainRuntime()
			if err := control.Validate(); err != nil {
				t.Fatalf("bound cleanup rejected: %v", err)
			}
			control.MountAttempt = 2
			if !errors.Is(control.Validate(), ErrInvalidRecord) {
				t.Fatal("cleanup mount greater than one accepted")
			}
		})
	}
}

func TestDomainCodecTypedNilAndFreshDestination(t *testing.T) {
	nilRecords := []interface{ Validate() error }{(*WorkspaceOwnerRecord)(nil), (*WorkspaceFenceRecord)(nil), (*SandboxControlRecord)(nil), (*SandboxSnapshotRecord)(nil), (*CreationIntentRecord)(nil), (*CreationRequestRecord)(nil), (*SandboxPlacementRecord)(nil)}
	for _, record := range nilRecords {
		if _, err := encodeDomainRecord(record); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("encode typed nil %T: %v", record, err)
		}
		if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(`{}`)}, record); !errors.Is(err, ErrCorruptRecord) {
			t.Errorf("decode typed nil %T: %v", record, err)
		}
	}
	owner := domainOwner()
	owner.Runtime = domainRuntime()
	wire, err := encodeDomainRecord(domainOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.Runtime != nil {
		t.Fatal("omitted runtime inherited old binding")
	}
	before := owner
	incomplete := strings.Replace(wire, `"generation":1,`, "", 1)
	if incomplete == wire {
		t.Fatal("fixture generation was not removed")
	}
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(incomplete)}, &owner); !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("missing generation inherited previous value: %v", err)
	}
	if owner != before {
		t.Fatal("failed decode mutated destination")
	}
}

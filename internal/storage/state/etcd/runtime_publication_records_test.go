package etcd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// A duplicate or omitted receipt field must fail even with an immutable envelope.
func TestPublicationReceiptStrictMetadata(t *testing.T) {
	r := dispatchRecord()
	s := stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeCommitted}
	wire := dispatchTestWire(t, s)
	for name, bad := range map[string]string{
		"duplicate": strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1),
		"missing":   strings.Replace(wire, `"partition":170,`, ``, 1),
		"null":      strings.Replace(wire, `"partition":170`, `"partition":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			l := r.Attempt
			if name != "duplicate" {
				l.Partition = 0
			}
			_, err := decodeRuntimeDispatchReceipt(&mvccpb.KeyValue{Value: []byte(bad), CreateRevision: 1, ModRevision: 1}, l)
			if !errors.Is(err, ErrCorruptReceipt) {
				t.Fatalf("strict receipt metadata accepted: %v", err)
			}
		})
	}
}

// The fixtures use a hand-computed digest of compact {}.
func publicationRecordFixture() RuntimePublicationRecord {
	d := dispatchRecord()
	d.Attempt.StageID = "runtime_publish"
	return RuntimePublicationRecord{Version: 1, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: 1, DataGateEpoch: 1, Snapshot: d.Snapshot, ExpiresAt: d.ExpiresAt, OperationID: dispatchUUID, PayloadDigest: domainHash, ProofDigest: "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a", CertificateDigest: domainHash, Runtime: RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}, Claim: d.Claim, Attempt: d.Attempt}
}
func publicationProofFixture(r RuntimePublicationRecord) RuntimePublicationProofRecord {
	return RuntimePublicationProofRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, ProofDigest: r.ProofDigest, Payload: json.RawMessage(`{}`)}
}
func TestRuntimePublicationCodec(t *testing.T) {
	r := publicationRecordFixture()
	p := publicationProofFixture(r)
	for _, record := range []interface{ Validate() error }{r, &r, p, &p} {
		wire, err := encodeDomainRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		kv := &mvccpb.KeyValue{Value: []byte(wire)}
		switch record.(type) {
		case RuntimePublicationRecord, *RuntimePublicationRecord:
			var got RuntimePublicationRecord
			if err := decodeDomainRecord(kv, &got); err != nil || got != r {
				t.Fatalf("journal roundtrip: %+v %v", got, err)
			}
		default:
			var got RuntimePublicationProofRecord
			if err := decodeDomainRecord(kv, &got); err != nil || string(got.Payload) != "{}" {
				t.Fatalf("proof roundtrip: %+v %v", got, err)
			}
			kv.Value[0] = 'x'
			if string(got.Payload) != "{}" {
				t.Fatal("decoder retained caller bytes")
			}
		}
	}
	var nr *RuntimePublicationRecord
	var np *RuntimePublicationProofRecord
	for _, record := range []interface{ Validate() error }{nr, np} {
		if _, err := encodeDomainRecord(record); !errors.Is(err, ErrInvalidRecord) {
			t.Fatal(err)
		}
	}
}
func TestRuntimePublicationValidation(t *testing.T) {
	base := publicationRecordFixture()
	for name, change := range map[string]func(*RuntimePublicationRecord){
		"version": func(r *RuntimePublicationRecord) { r.Version = 2 }, "generation": func(r *RuntimePublicationRecord) { r.Generation = 0 }, "gate": func(r *RuntimePublicationRecord) { r.DataGateEpoch = 0 },
		"intent": func(r *RuntimePublicationRecord) { r.IntentID = "../x" }, "sandbox": func(r *RuntimePublicationRecord) { r.SandboxID = "" }, "hash": func(r *RuntimePublicationRecord) { r.WorkspaceHash = "x" }, "restore": func(r *RuntimePublicationRecord) { r.RestoreEpoch = "" },
		"operation nil": func(r *RuntimePublicationRecord) { r.OperationID = "00000000-0000-0000-0000-000000000000" }, "claim nil": func(r *RuntimePublicationRecord) { r.Claim.ClaimID = "00000000-0000-0000-0000-000000000000" }, "attempt nil": func(r *RuntimePublicationRecord) { r.Attempt.AttemptID = "00000000-0000-0000-0000-000000000000" },
		"payload digest": func(r *RuntimePublicationRecord) { r.PayloadDigest = strings.Repeat("A", 64) }, "proof digest": func(r *RuntimePublicationRecord) { r.ProofDigest = "" }, "certificate digest": func(r *RuntimePublicationRecord) { r.CertificateDigest = "" },
		"snapshot": func(r *RuntimePublicationRecord) { r.Snapshot.Version = "" }, "expiry": func(r *RuntimePublicationRecord) { r.ExpiresAt = time.Time{} }, "zone": func(r *RuntimePublicationRecord) { r.ExpiresAt = r.ExpiresAt.In(time.FixedZone("east", 3600)) },
		"boot": func(r *RuntimePublicationRecord) { r.Runtime.BootID = "" }, "worker": func(r *RuntimePublicationRecord) { r.Claim.WorkerID = "" }, "claim revision": func(r *RuntimePublicationRecord) { r.Claim.CreateRevision = 0 }, "claim lease": func(r *RuntimePublicationRecord) { r.Claim.LeaseID = 0 },
		"partition": func(r *RuntimePublicationRecord) { r.Attempt.Partition++ }, "attempt restore": func(r *RuntimePublicationRecord) { r.Attempt.RestoreEpoch = "old" }, "stage": func(r *RuntimePublicationRecord) { r.Attempt.StageID = "runtime_dispatch" }, "namespace": func(r *RuntimePublicationRecord) { r.Attempt.Namespace = "invalid" },
		"mount two": func(r *RuntimePublicationRecord) { r.MountAttempt = 2 }, "mount missing": func(r *RuntimePublicationRecord) { r.MountAttempt = 1 }, "plain mount operation": func(r *RuntimePublicationRecord) { r.MountOperationID = dispatchUUID }, "mount nil": func(r *RuntimePublicationRecord) {
			r.MountAttempt = 1
			r.MountOperationID = "00000000-0000-0000-0000-000000000000"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			change(&r)
			if _, err := encodeDomainRecord(r); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("invalid journal accepted: %v", err)
			}
		})
	}
	base.MountAttempt = 1
	base.MountOperationID = dispatchUUID
	if _, err := encodeDomainRecord(base); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimePublicationProofBudgetsAndOpaqueJSON(t *testing.T) {
	p := publicationProofFixture(publicationRecordFixture())
	for _, payload := range []string{`null`, `[1,true,"<>&"]`, `{"x":null,"x":2}`} {
		p.Payload = []byte(payload)
		p.ProofDigest, _ = snapshotDigest(p.Payload)
		wire, err := encodeDomainRecord(p)
		if err != nil {
			t.Fatal(err)
		}
		var got RuntimePublicationProofRecord
		if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &got); err != nil {
			t.Fatal(err)
		}
	}
	p = publicationProofFixture(publicationRecordFixture())
	p.Payload = []byte("{" + strings.Repeat(" ", 8190) + "}")
	if _, err := encodeDomainRecord(p); err != nil {
		t.Fatalf("8192 raw wire rejected: %v", err)
	}
	p.Payload = append(p.Payload, ' ')
	if _, err := encodeDomainRecord(p); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("8193 whitespace wire accepted: %v", err)
	}
	wire := dispatchTestWire(t, publicationProofFixture(publicationRecordFixture()))
	// Raw payload whitespace survives decoding; its budget precedes compaction.
	for _, bad := range []string{strings.Replace(wire, `"payload":{}`, `"payload":{`+strings.Repeat(" ", 8191)+`}`, 1), wire + strings.Repeat(" ", 16385-len(wire))} {
		var got RuntimePublicationProofRecord
		if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(bad)}, &got); !errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("oversize stored proof accepted: %v", err)
		}
	}
	boundary := wire + strings.Repeat(" ", 16384-len(wire))
	var got RuntimePublicationProofRecord
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(boundary)}, &got); err != nil {
		t.Fatalf("16384 record boundary: %v", err)
	}
}
func TestRuntimePublicationStrictOuterMetadata(t *testing.T) {
	for _, record := range []interface{ Validate() error }{publicationRecordFixture(), publicationProofFixture(publicationRecordFixture())} {
		wire := dispatchTestWire(t, record)
		bads := []string{strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(wire, `"version":1,`, ``, 1), strings.Replace(wire, `"version":1`, `"version":null`, 1), strings.TrimSuffix(wire, "}") + `,"unknown":1}`, wire + ` {}`, "null", wire + "\xff"}
		if _, ok := record.(RuntimePublicationRecord); ok {
			bads = append(bads, strings.Replace(wire, `"boot_id":"boot"`, `"boot_id":null`, 1), strings.Replace(wire, `"lease_id":1`, `"lease_id":1,"lease_id":1`, 1))
		}
		for _, bad := range bads {
			var err error
			if _, ok := record.(RuntimePublicationRecord); ok {
				var got RuntimePublicationRecord
				err = decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(bad)}, &got)
			} else {
				var got RuntimePublicationProofRecord
				err = decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(bad)}, &got)
			}
			if !errors.Is(err, ErrCorruptRecord) {
				t.Fatalf("bad metadata accepted: %s: %v", bad, err)
			}
		}
	}
}

func TestRuntimePublicationProofRejectsInvalidInputAndOwnsBytes(t *testing.T) {
	base := publicationProofFixture(publicationRecordFixture())
	for name, change := range map[string]func(*RuntimePublicationProofRecord){
		"version":     func(p *RuntimePublicationProofRecord) { p.Version = 0 },
		"generation":  func(p *RuntimePublicationProofRecord) { p.Generation = -1 },
		"workspace":   func(p *RuntimePublicationProofRecord) { p.WorkspaceHash = "bad" },
		"intent":      func(p *RuntimePublicationProofRecord) { p.IntentID = "../bad" },
		"sandbox":     func(p *RuntimePublicationProofRecord) { p.SandboxID = "" },
		"restore":     func(p *RuntimePublicationProofRecord) { p.RestoreEpoch = "" },
		"digest":      func(p *RuntimePublicationProofRecord) { p.ProofDigest = domainHash },
		"nil payload": func(p *RuntimePublicationProofRecord) { p.Payload = nil },
		"malformed":   func(p *RuntimePublicationProofRecord) { p.Payload = []byte(`{`) },
		"trailing":    func(p *RuntimePublicationProofRecord) { p.Payload = []byte(`{} {}`) },
		"utf8":        func(p *RuntimePublicationProofRecord) { p.Payload = []byte("\"\xff\"") },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			change(&p)
			if _, err := encodeDomainRecord(p); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("invalid proof accepted: %v", err)
			}
		})
	}
	wire, err := encodeDomainRecord(&base)
	if err != nil {
		t.Fatal(err)
	}
	base.Payload[0] = 'x'
	var got RuntimePublicationProofRecord
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &got); err != nil || string(got.Payload) != "{}" {
		t.Fatal(got, err)
	}
	before := got
	if err := decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(`{}`)}, &got); !errors.Is(err, ErrCorruptRecord) || got.ProofDigest != before.ProofDigest || string(got.Payload) != string(before.Payload) {
		t.Fatal("failed decode mutated destination", err)
	}
}

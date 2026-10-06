package controltarget

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func journalIdentityFixture() JournalIdentity {
	return JournalIdentity{Namespace: "/sandbox/control/prod/", AuthorityID: "authority<&>", Target: "target", RestoreEpoch: "restore-1", SandboxID: "sandbox-1", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: controlprotocol.RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}}
}
func journalRecordFixture() ExecJournalRecord {
	i := journalIdentityFixture()
	end := time.Date(2026, 10, 7, 1, 0, 30, 123456789, time.UTC)
	return ExecJournalRecord{Version: 1, State: "unknown", Context: controlprotocol.ExecStartContext{Namespace: i.Namespace, AuthorityID: i.AuthorityID, Target: i.Target, RestoreEpoch: i.RestoreEpoch, IssuerCertificateID: "11111111-1111-4111-8111-111111111111", IssuerCertificateDigest: strings.Repeat("b", 64), CommandID: "22222222-2222-4222-8222-222222222222", OperationID: "33333333-3333-4333-8333-333333333333", RequestID: "request-1", OperationDigest: strings.Repeat("c", 64), SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: 1, DataGateEpoch: 2, ControlRevision: 3, AdmissionRevision: 4, LeaseID: 5, Runtime: i.Runtime, ExpiresAt: end}, DescriptorDigest: strings.Repeat("d", 64), TicketDigest: strings.Repeat("e", 64), NotBefore: end.Add(-30 * time.Second), NotAfter: end}
}

func TestJournalIdentityValidation(t *testing.T) {
	if err := journalIdentityFixture().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*JournalIdentity){
		"namespace relative":             func(i *JournalIdentity) { i.Namespace = "sandbox/a/b/" },
		"namespace short":                func(i *JournalIdentity) { i.Namespace = "/a/b/" },
		"namespace empty segment":        func(i *JournalIdentity) { i.Namespace = "/a//b/" },
		"namespace too long":             func(i *JournalIdentity) { i.Namespace = "/" + strings.Repeat("a", 508) + "/b/c/" },
		"namespace penultimate too long": func(i *JournalIdentity) { i.Namespace = "/a/" + strings.Repeat("b", 129) + "/c/" },
		"namespace trailing":             func(i *JournalIdentity) { i.Namespace = "/a/b/c" },
		"authority empty":                func(i *JournalIdentity) { i.AuthorityID = "" },
		"authority too long":             func(i *JournalIdentity) { i.AuthorityID = strings.Repeat("a", 129) },
		"authority invalid utf8":         func(i *JournalIdentity) { i.AuthorityID = string([]byte{255}) },
		"target control":                 func(i *JournalIdentity) { i.Target = "x\n" },
		"restore dot":                    func(i *JournalIdentity) { i.RestoreEpoch = ".." },
		"sandbox slash":                  func(i *JournalIdentity) { i.SandboxID = "a/b" },
		"sandbox too long":               func(i *JournalIdentity) { i.SandboxID = strings.Repeat("a", 129) },
		"hash uppercase":                 func(i *JournalIdentity) { i.WorkspaceHash = strings.Repeat("A", 64) },
		"hash short":                     func(i *JournalIdentity) { i.WorkspaceHash = "a" },
		"generation zero":                func(i *JournalIdentity) { i.Generation = 0 },
		"generation negative":            func(i *JournalIdentity) { i.Generation = -1 },
		"runtime id":                     func(i *JournalIdentity) { i.Runtime.ID = "" },
		"runtime uid":                    func(i *JournalIdentity) { i.Runtime.UID = "\x00" },
		"runtime boot":                   func(i *JournalIdentity) { i.Runtime.BootID = strings.Repeat("a", 129) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			i := journalIdentityFixture()
			mutate(&i)
			if !errors.Is(i.Validate(), ErrInvalidRecord) {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}

func TestJournalIdentityManifestValidation(t *testing.T) {
	valid := GateManifest{Version: 1, Identity: journalIdentityFixture(), DataGateEpoch: 2, GateState: "closed"}
	for _, state := range []string{"closed", "open"} {
		m := valid
		m.GateState = state
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(*GateManifest){"version": func(m *GateManifest) { m.Version = 2 }, "identity": func(m *GateManifest) { m.Identity.Target = "" }, "epoch": func(m *GateManifest) { m.DataGateEpoch = 0 }, "state": func(m *GateManifest) { m.GateState = "accepted" }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := valid
			mutate(&m)
			if !errors.Is(m.Validate(), ErrInvalidRecord) {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestJournalIdentityRecordValidation(t *testing.T) {
	if err := journalRecordFixture().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*ExecJournalRecord){
		"version": func(r *ExecJournalRecord) { r.Version = 0 }, "state": func(r *ExecJournalRecord) { r.State = "started" },
		"namespace": func(r *ExecJournalRecord) { r.Context.Namespace = "/a/b/" }, "authority": func(r *ExecJournalRecord) { r.Context.AuthorityID = "" }, "target": func(r *ExecJournalRecord) { r.Context.Target = "\x7f" }, "restore": func(r *ExecJournalRecord) { r.Context.RestoreEpoch = "." },
		"issuer uuid": func(r *ExecJournalRecord) { r.Context.IssuerCertificateID = "00000000-0000-0000-0000-000000000000" }, "issuer digest": func(r *ExecJournalRecord) { r.Context.IssuerCertificateDigest = "x" },
		"command uuid": func(r *ExecJournalRecord) { r.Context.CommandID = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA" }, "operation uuid": func(r *ExecJournalRecord) { r.Context.OperationID = "33333333333343338333333333333333" },
		"request": func(r *ExecJournalRecord) { r.Context.RequestID = "a/b" }, "operation digest": func(r *ExecJournalRecord) { r.Context.OperationDigest = strings.Repeat("F", 64) }, "sandbox": func(r *ExecJournalRecord) { r.Context.SandboxID = "" }, "workspace hash": func(r *ExecJournalRecord) { r.Context.WorkspaceHash = "" },
		"generation": func(r *ExecJournalRecord) { r.Context.Generation = 0 }, "gate epoch": func(r *ExecJournalRecord) { r.Context.DataGateEpoch = -1 }, "control revision": func(r *ExecJournalRecord) { r.Context.ControlRevision = 0 }, "admission revision": func(r *ExecJournalRecord) { r.Context.AdmissionRevision = 0 }, "lease": func(r *ExecJournalRecord) { r.Context.LeaseID = 0 },
		"runtime id": func(r *ExecJournalRecord) { r.Context.Runtime.ID = "" }, "runtime uid": func(r *ExecJournalRecord) { r.Context.Runtime.UID = "" }, "runtime boot": func(r *ExecJournalRecord) { r.Context.Runtime.BootID = "" },
		"expires zero": func(r *ExecJournalRecord) { r.Context.ExpiresAt = time.Time{} }, "expires non utc": func(r *ExecJournalRecord) { r.Context.ExpiresAt = r.Context.ExpiresAt.In(time.FixedZone("offset", 0)) },
		"descriptor": func(r *ExecJournalRecord) { r.DescriptorDigest = "x" }, "ticket": func(r *ExecJournalRecord) { r.TicketDigest = "x" },
		"before zero": func(r *ExecJournalRecord) { r.NotBefore = time.Time{} }, "after zero": func(r *ExecJournalRecord) { r.NotAfter = time.Time{} }, "before non utc": func(r *ExecJournalRecord) { r.NotBefore = r.NotBefore.In(time.FixedZone("UTC", 0)) }, "after non utc": func(r *ExecJournalRecord) { r.NotAfter = r.NotAfter.In(time.FixedZone("UTC", 0)) },
		"equal window": func(r *ExecJournalRecord) { r.NotBefore = r.NotAfter }, "reversed window": func(r *ExecJournalRecord) { r.NotBefore = r.NotAfter.Add(time.Second) }, "long window": func(r *ExecJournalRecord) { r.NotBefore = r.NotBefore.Add(-time.Nanosecond) }, "business expiration": func(r *ExecJournalRecord) { r.Context.ExpiresAt = r.NotAfter.Add(-time.Nanosecond) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := journalRecordFixture()
			mutate(&r)
			if !errors.Is(r.Validate(), ErrInvalidRecord) {
				t.Fatal("invalid record accepted")
			}
		})
	}
}

// Removing any schema check must admit at least one of these malformed fields.
// The test discovers every field from the real typed fixture, including nested
// context/runtime fields, then mutates only that field's wire representation.
func TestJournalCodecStrictFields(t *testing.T) {
	manifest := GateManifest{Version: 1, Identity: journalIdentityFixture(), DataGateEpoch: 2, GateState: "closed"}
	for _, tc := range []struct {
		name   string
		value  any
		decode func([]byte) error
	}{
		{"manifest", manifest, func(w []byte) error {
			dst := manifest
			before := dst
			err := decodeGateManifest(w, &dst)
			if dst != before {
				t.Fatal("failed decode changed manifest")
			}
			return err
		}},
		{"record", journalRecordFixture(), func(w []byte) error {
			dst := journalRecordFixture()
			before := dst
			err := decodeExecJournalRecord(w, &dst)
			if dst != before {
				t.Fatal("failed decode changed record")
			}
			return err
		}},
	} {
		wire, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		var tree map[string]any
		if err := json.Unmarshal(wire, &tree); err != nil {
			t.Fatal(err)
		}
		var visit func(map[string]any, []string)
		visit = func(node map[string]any, path []string) {
			for key, value := range node {
				fieldPath := append(append([]string(nil), path...), key)
				for _, bad := range []string{"missing", "null", "wrong_type", "case", "duplicate"} {
					t.Run(tc.name+"/"+strings.Join(fieldPath, ".")+"/"+bad, func(t *testing.T) {
						var copyTree map[string]any
						if err := json.Unmarshal(wire, &copyTree); err != nil {
							t.Fatal(err)
						}
						parent := copyTree
						for _, p := range path {
							parent = parent[p].(map[string]any)
						}
						switch bad {
						case "missing":
							delete(parent, key)
						case "null":
							parent[key] = nil
						case "wrong_type":
							switch value.(type) {
							case string:
								parent[key] = false
							case float64:
								parent[key] = "1"
							default:
								parent[key] = []any{}
							}
						case "case":
							delete(parent, key)
							parent[strings.ToUpper(key)] = value
						}
						malformed, err := json.Marshal(copyTree)
						if err != nil {
							t.Fatal(err)
						}
						if bad == "duplicate" {
							v, err := json.Marshal(value)
							if err != nil {
								t.Fatal(err)
							}
							needle := []byte(`"` + key + `":`)
							replacement := append(append(append([]byte(nil), needle...), v...), ',')
							replacement = append(replacement, needle...)
							malformed = bytes.Replace(malformed, needle, replacement, 1)
						}
						if err := tc.decode(malformed); !errors.Is(err, ErrInvalidRecord) {
							t.Fatalf("malformed field accepted: %v", err)
						}
					})
				}
				if child, ok := value.(map[string]any); ok {
					visit(child, fieldPath)
				}
			}
			t.Run(tc.name+"/"+strings.Join(path, ".")+"/unknown", func(t *testing.T) {
				var clone map[string]any
				if err := json.Unmarshal(wire, &clone); err != nil {
					t.Fatal(err)
				}
				parent := clone
				for _, p := range path {
					parent = parent[p].(map[string]any)
				}
				parent["extra"] = true
				bad, err := json.Marshal(clone)
				if err != nil {
					t.Fatal(err)
				}
				if !errors.Is(tc.decode(bad), ErrInvalidRecord) {
					t.Fatal("unknown field accepted")
				}
			})
		}
		visit(tree, nil)
	}
}

func TestJournalCodecBoundaries(t *testing.T) {
	m := GateManifest{Version: 1, Identity: journalIdentityFixture(), DataGateEpoch: 2, GateState: "closed"}
	mw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r := journalRecordFixture()
	rw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		wire   []byte
		decode func([]byte) error
	}{
		{"manifest", mw, func(w []byte) error {
			dst := m
			err := decodeGateManifest(w, &dst)
			if err != nil && dst != m {
				t.Fatal("failed decode changed destination")
			}
			return err
		}},
		{"record", rw, func(w []byte) error {
			dst := r
			err := decodeExecJournalRecord(w, &dst)
			if err != nil && dst != r {
				t.Fatal("failed decode changed destination")
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			padded := append(append([]byte(nil), tc.wire...), bytes.Repeat([]byte(" "), 8192-len(tc.wire))...)
			if err := tc.decode(padded); err != nil {
				t.Fatalf("8192 rejected: %v", err)
			}
			bads := map[string][]byte{"8193": append(padded, ' '), "empty": {}, "null": []byte("null"), "trailing object": append(append([]byte(nil), tc.wire...), []byte(" {}")...), "trailing garbage": append(append([]byte(nil), tc.wire...), 'x'), "invalid utf8": bytes.Replace(tc.wire, []byte("target"), []byte{255}, 1), "depth 9": []byte(`{"identity":{"runtime":{"a":{"b":{"c":{"d":{"e":{"f":{"g":1}}}}}}}}}`), "unpaired high": bytes.Replace(tc.wire, []byte("authority<&>"), []byte(`\ud800`), 1), "unpaired low": bytes.Replace(tc.wire, []byte("authority<&>"), []byte(`\udc00`), 1), "wrong version": bytes.Replace(tc.wire, []byte(`"version":1`), []byte(`"version":2`), 1), "uint32 overflow": bytes.Replace(tc.wire, []byte(`"version":1`), []byte(`"version":4294967296`), 1), "int64 overflow": bytes.Replace(tc.wire, []byte(`"generation":1`), []byte(`"generation":9223372036854775808`), 1), "float integer": bytes.Replace(tc.wire, []byte(`"generation":1`), []byte(`"generation":1.0`), 1), "exponent integer": bytes.Replace(tc.wire, []byte(`"generation":1`), []byte(`"generation":1e0`), 1)}
			// Marshaling escapes HTML; target the literal escape sequence as well.
			bads["unpaired high"] = bytes.Replace(tc.wire, []byte(`authority\u003c\u0026\u003e`), []byte(`\ud800`), 1)
			bads["unpaired low"] = bytes.Replace(tc.wire, []byte(`authority\u003c\u0026\u003e`), []byte(`\udc00`), 1)
			for name, bad := range bads {
				t.Run(name, func(t *testing.T) {
					if !errors.Is(tc.decode(bad), ErrInvalidRecord) {
						t.Fatal("invalid wire accepted")
					}
				})
			}
		})
	}
	if !errors.Is(decodeGateManifest(mw, nil), ErrInvalidRecord) || !errors.Is(decodeExecJournalRecord(rw, nil), ErrInvalidRecord) {
		t.Fatal("nil destination accepted")
	}
}

func TestJournalCodecCanonical(t *testing.T) {
	m := GateManifest{Version: 1, Identity: journalIdentityFixture(), DataGateEpoch: 2, GateState: "closed"}
	mw, err := encodeGateManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(mw, []byte(`\u003c`)) || bytes.Contains(mw, []byte("\n")) {
		t.Fatal("encoding escaped HTML or added newline")
	}
	var actual GateManifest
	if err := decodeGateManifest(mw, &actual); err != nil || actual != m {
		t.Fatalf("manifest roundtrip: %v", err)
	}
	actual.GateState = "open"
	historical, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeGateManifest(historical, &actual); err != nil {
		t.Fatal(err)
	}
	if _, err := encodeGateManifest(actual); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal("encoder produced open gate")
	}
	invalid := m
	invalid.Identity.Target = ""
	if _, err := encodeGateManifest(invalid); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal("encoded invalid manifest")
	}
	r := journalRecordFixture()
	rw, err := encodeExecJournalRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	var got ExecJournalRecord
	if err := decodeExecJournalRecord(rw, &got); err != nil || got != r {
		t.Fatalf("record roundtrip: %v", err)
	}
	got.Context.Target = "changed"
	if r.Context.Target != "target" {
		t.Fatal("record not owned")
	}
	for _, forbidden := range []string{`"ticket":`, `"argv":`, `"env":`, `"stdin":`, `"stdout":`, `"files":`, `"payload":`} {
		if bytes.Contains(rw, []byte(forbidden)) {
			t.Fatalf("persisted payload %s", forbidden)
		}
	}
	invalidR := r
	invalidR.State = "completed"
	if _, err := encodeExecJournalRecord(invalidR); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal("encoded invalid record")
	}
	for name, value := range map[string]string{"offset": "2026-10-07T01:00:30.123456789+00:00", "zero": "0001-01-01T00:00:00Z", "fraction zeros": "2026-10-07T01:00:30.1234567890Z", "comma fraction": "2026-10-07T01:00:30,123456789Z", "invalid date": "2026-99-07T01:00:30Z"} {
		t.Run(name, func(t *testing.T) {
			bad := bytes.Replace(rw, []byte(`"expires_at":"2026-10-07T01:00:30.123456789Z"`), []byte(`"expires_at":"`+value+`"`), 1)
			dst := r
			if err := decodeExecJournalRecord(bad, &dst); !errors.Is(err, ErrInvalidRecord) || dst != r {
				t.Fatalf("invalid time accepted/changed: %v", err)
			}
		})
	}
	altered := r
	altered.NotBefore = altered.NotBefore.Add(time.Nanosecond)
	other, err := encodeExecJournalRecord(altered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rw, other) {
		t.Fatal("different window produced same canonical bytes")
	}
}

func TestJournalCodecMaxTypedFields(t *testing.T) {
	r := journalRecordFixture()
	r.Context.Namespace = "/" + strings.Repeat("a", 252) + "/" + strings.Repeat("b", 128) + "/" + strings.Repeat("c", 128) + "/"
	opaque := strings.Repeat(`"`, 128)
	r.Context.AuthorityID = opaque
	r.Context.Target = opaque
	r.Context.RestoreEpoch = strings.Repeat("a", 128)
	r.Context.SandboxID = strings.Repeat("a", 128)
	r.Context.RequestID = strings.Repeat("a", 128)
	r.Context.Runtime = controlprotocol.RuntimeReference{ID: opaque, UID: opaque, BootID: opaque}
	r.Context.Generation = math.MaxInt64
	r.Context.DataGateEpoch = math.MaxInt64
	r.Context.ControlRevision = math.MaxInt64
	r.Context.AdmissionRevision = math.MaxInt64
	r.Context.LeaseID = math.MaxInt64
	r.Context.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	r.NotAfter = r.Context.ExpiresAt
	r.NotBefore = r.NotAfter.Add(-30 * time.Second)
	w, err := encodeExecJournalRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	var got ExecJournalRecord
	if err := decodeExecJournalRecord(w, &got); err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("max record: %v", err)
	}
	i := JournalIdentity{Namespace: r.Context.Namespace, AuthorityID: opaque, Target: opaque, RestoreEpoch: r.Context.RestoreEpoch, SandboxID: r.Context.SandboxID, WorkspaceHash: r.Context.WorkspaceHash, Generation: math.MaxInt64, Runtime: r.Context.Runtime}
	m := GateManifest{Version: 1, Identity: i, DataGateEpoch: math.MaxInt64, GateState: "closed"}
	mw, err := encodeGateManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	var gm GateManifest
	if err := decodeGateManifest(mw, &gm); err != nil || gm != m {
		t.Fatalf("max manifest: %v", err)
	}
	t.Logf("max-field structural sample record=%d manifest=%d namespace=%d time=%d", len(w), len(mw), len(i.Namespace), len(r.NotAfter.Format(time.RFC3339Nano)))
	for _, year := range []int{-1, 10000} {
		bad := r
		bad.Context.ExpiresAt = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		if !errors.Is(bad.Validate(), ErrInvalidRecord) {
			t.Fatal("unencodable year accepted")
		}
	}
}

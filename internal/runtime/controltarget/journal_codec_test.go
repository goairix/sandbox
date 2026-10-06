package controltarget

import (
	"errors"
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

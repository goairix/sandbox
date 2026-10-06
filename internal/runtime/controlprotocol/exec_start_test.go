package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testExecStartDomain = "sandbox-exec-start-ticket:v1\x00"

type execStartFixture struct {
	root                 ed25519.PublicKey
	rootKey, delegateKey ed25519.PrivateKey
	certificate          CommandIssuerCertificateClaims
	issuer               CommandIssuerIdentity
	issuerWire           []byte
	verifier             *ManagementVerifier
	descriptor           ExecutionDescriptor
	claims               ExecStartTicketClaims
	now                  time.Time
}

func execStartSetup(t *testing.T) execStartFixture {
	t.Helper()
	root, rootKey, certificate, now := managementTestClaims(t)
	delegate, delegateKey := managementTestKey(t)
	certificate.PublicKey = delegate
	verifier, err := NewManagementVerifier(managementTestBinding(certificate), []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	issuerWire, err := SignCommandIssuerCertificate(rootKey, certificate)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := verifier.VerifyCommandIssuerCertificate(issuerWire, now)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := NewExecutionDescriptor(ExecutionRequest{Argv: []string{"/bin/tool", "raw; $argument"}, Env: map[string]string{"PATH": "/usr/bin"}, UID: 1000, GID: 1001, WorkDir: "/work", TimeoutSeconds: 10, Stdin: []byte{0, 255, 10}, TTY: true, RequiresNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	context := ExecStartContext{Namespace: certificate.Namespace, AuthorityID: certificate.AuthorityID, Target: certificate.Target, RestoreEpoch: certificate.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: "8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", OperationID: "9a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", RequestID: "request.not-a-uuid", OperationDigest: strings.Repeat("a", 64), SandboxID: "sandbox-1", WorkspaceHash: strings.Repeat("b", 64), Generation: 2, DataGateEpoch: 3, ControlRevision: 4, AdmissionRevision: 5, LeaseID: 6, Runtime: RuntimeReference{ID: "runtime", UID: "runtime-uid", BootID: "boot-1"}, ExpiresAt: now.Add(time.Minute)}
	return execStartFixture{root: root, rootKey: rootKey, delegateKey: delegateKey, certificate: certificate, issuer: issuer, issuerWire: issuerWire, verifier: verifier, descriptor: descriptor, claims: ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: context, DescriptorDigest: descriptor.Digest(), NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)}, now: now}
}

// Independently signed envelopes exercise verification beyond signer prevalidation.
func execStartRaw(t *testing.T, key ed25519.PrivateKey, claims ExecStartTicketClaims, domain string) []byte {
	t.Helper()
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(struct {
		Claims    ExecStartTicketClaims `json:"claims"`
		Signature []byte                `json:"signature"`
	}{claims, ed25519.Sign(key, append([]byte(domain), encoded...))})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
func execStartReject(t *testing.T, v *ManagementVerifier, ticket, issuer []byte, expected ExecStartContext, descriptor ExecutionDescriptor, now time.Time) {
	t.Helper()
	evidence, err := v.VerifyExecStartTicket(ticket, issuer, expected, descriptor, now)
	if err == nil {
		t.Fatal("accepted invalid exec start ticket")
	}
	if !reflect.DeepEqual(evidence, ExecStartEvidence{}) {
		t.Fatalf("partial evidence on error: %+v", evidence)
	}
}
func execStartSignReject(t *testing.T, key ed25519.PrivateKey, issuer CommandIssuerIdentity, claims ExecStartTicketClaims) {
	t.Helper()
	wire, err := SignExecStartTicket(key, issuer, claims)
	if err == nil || wire != nil {
		t.Fatal("signed invalid exec start ticket")
	}
}

func TestExecStartTicket(t *testing.T) {
	f := execStartSetup(t)
	wire, err := SignExecStartTicket(f.delegateKey, f.issuer, f.claims)
	if err != nil {
		t.Fatalf("real delegate could not sign bounded original-operation start: %v", err)
	}
	original := append([]byte(nil), wire...)
	var envelope struct {
		Claims    ExecStartTicketClaims `json:"claims"`
		Signature []byte                `json:"signature"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatal(err)
	}
	signed, _ := json.Marshal(envelope.Claims)
	if !ed25519.Verify(f.issuer.PublicKey(), append([]byte(testExecStartDomain), signed...), envelope.Signature) {
		t.Fatal("ticket does not use fixed actual-NUL start domain")
	}
	if !reflect.DeepEqual(envelope.Claims, f.claims) {
		t.Fatal("signer derived or changed context/time/payload")
	}
	evidence, err := f.verifier.VerifyExecStartTicket(wire, f.issuerWire, f.claims.Context, f.descriptor, f.now)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)
	if evidence.Digest() != hex.EncodeToString(digest[:]) || evidence.Context() != f.claims.Context || evidence.DescriptorDigest() != f.descriptor.Digest() || !evidence.NotBefore().Equal(f.claims.NotBefore) || !evidence.NotAfter().Equal(f.claims.NotAfter) {
		t.Fatal("evidence getters do not bind actual authenticated claims")
	}
	wire[0] ^= 1
	copied := evidence.Wire()
	copied[0] ^= 1
	context := evidence.Context()
	context.Runtime.BootID = "changed"
	f.delegateKey[0] ^= 1
	if !bytes.Equal(evidence.Wire(), original) || evidence.Context() != f.claims.Context {
		t.Fatal("caller mutation changed evidence")
	}
	pretty := new(bytes.Buffer)
	if err := json.Indent(pretty, original, "", " "); err != nil {
		t.Fatal(err)
	}
	normalized, err := f.verifier.VerifyExecStartTicket(pretty.Bytes(), f.issuerWire, f.claims.Context, f.descriptor, f.now)
	if err != nil || normalized.Digest() != evidence.Digest() || !bytes.Equal(normalized.Wire(), original) {
		t.Fatalf("normalization: %v", err)
	}
	for _, now := range []time.Time{f.claims.NotBefore.Add(time.Second), f.claims.NotAfter.Add(-time.Second - time.Nanosecond)} {
		if _, err := f.verifier.VerifyExecStartTicket(original, f.issuerWire, f.claims.Context, f.descriptor, now); err != nil {
			t.Fatalf("conservative accepted boundary rejected: %v", err)
		}
	}
	// Reconstruct the same known UTC expiry independently of its Go representation.
	expected := f.claims.Context
	expected.ExpiresAt = time.Unix(f.claims.Context.ExpiresAt.Unix(), int64(f.claims.Context.ExpiresAt.Nanosecond())).UTC()
	if _, err := f.verifier.VerifyExecStartTicket(original, f.issuerWire, expected, f.descriptor, f.now); err != nil {
		t.Fatalf("same UTC instant rejected: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				e, err := f.verifier.VerifyExecStartTicket(original, f.issuerWire, f.claims.Context, f.descriptor, f.now)
				if err != nil {
					t.Errorf("concurrent verify: %v", err)
					return
				}
				w := e.Wire()
				w[0] ^= 1
				if !bytes.Equal(e.Wire(), original) {
					t.Error("concurrent getter leaked mutable wire")
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("actual issuer=%dB ticket=%dB descriptor metadata=%dB; ticket contains no stdin/stdout", len(f.issuerWire), len(original), len(f.descriptor.Canonical()))
}

func execStartContextMutations() []struct {
	name   string
	change func(*ExecStartContext)
} {
	return []struct {
		name   string
		change func(*ExecStartContext)
	}{
		{"namespace", func(c *ExecStartContext) { c.Namespace = "/sandbox/control/other/" }},
		{"authority", func(c *ExecStartContext) { c.AuthorityID = "other" }},
		{"target", func(c *ExecStartContext) { c.Target = "other" }},
		{"restore", func(c *ExecStartContext) { c.RestoreEpoch = "other" }},
		{"issuer-id", func(c *ExecStartContext) { c.IssuerCertificateID = c.CommandID }},
		{"issuer-digest", func(c *ExecStartContext) { c.IssuerCertificateDigest = strings.Repeat("c", 64) }},
		{"command", func(c *ExecStartContext) { c.CommandID = c.OperationID }},
		{"operation", func(c *ExecStartContext) { c.OperationID = c.CommandID }},
		{"request", func(c *ExecStartContext) { c.RequestID = "request-other" }},
		{"operation-digest", func(c *ExecStartContext) { c.OperationDigest = strings.Repeat("c", 64) }},
		{"sandbox", func(c *ExecStartContext) { c.SandboxID = "sandbox-other" }},
		{"workspace", func(c *ExecStartContext) { c.WorkspaceHash = strings.Repeat("c", 64) }},
		{"generation", func(c *ExecStartContext) { c.Generation++ }},
		{"gate", func(c *ExecStartContext) { c.DataGateEpoch++ }},
		{"control", func(c *ExecStartContext) { c.ControlRevision++ }},
		{"admission", func(c *ExecStartContext) { c.AdmissionRevision++ }},
		{"lease", func(c *ExecStartContext) { c.LeaseID++ }},
		{"runtime-id", func(c *ExecStartContext) { c.Runtime.ID = "other" }},
		{"runtime-uid", func(c *ExecStartContext) { c.Runtime.UID = "other" }},
		{"runtime-boot", func(c *ExecStartContext) { c.Runtime.BootID = "other" }},
		{"expiry", func(c *ExecStartContext) { c.ExpiresAt = c.ExpiresAt.Add(time.Second) }},
	}
}

func TestExecStartTicketRejects(t *testing.T) {
	f := execStartSetup(t)
	valid := execStartRaw(t, f.delegateKey, f.claims, testExecStartDomain)
	for _, tc := range execStartContextMutations() {
		t.Run("expected-"+tc.name, func(t *testing.T) {
			expected := f.claims.Context
			tc.change(&expected)
			execStartReject(t, f.verifier, valid, f.issuerWire, expected, f.descriptor, f.now)
		})
		t.Run("signed-context-"+tc.name, func(t *testing.T) {
			c := f.claims
			tc.change(&c.Context)
			execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, c, testExecStartDomain), f.issuerWire, f.claims.Context, f.descriptor, f.now)
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*ExecutionRequest)
	}{
		{"argv", func(r *ExecutionRequest) { r.Argv[1] += "x" }}, {"env", func(r *ExecutionRequest) { r.Env["PATH"] += "x" }}, {"uid", func(r *ExecutionRequest) { r.UID++ }}, {"gid", func(r *ExecutionRequest) { r.GID++ }}, {"workdir", func(r *ExecutionRequest) { r.WorkDir += "x" }}, {"timeout", func(r *ExecutionRequest) { r.TimeoutSeconds++ }}, {"stdin", func(r *ExecutionRequest) { r.Stdin[0]++ }}, {"stdin-length", func(r *ExecutionRequest) { r.Stdin = append(r.Stdin, 0) }}, {"tty", func(r *ExecutionRequest) { r.TTY = !r.TTY }}, {"network", func(r *ExecutionRequest) { r.RequiresNetwork = !r.RequiresNetwork }},
	} {
		t.Run("payload-"+tc.name, func(t *testing.T) {
			r := f.descriptor.Request()
			tc.change(&r)
			d, err := NewExecutionDescriptor(r)
			if err != nil {
				t.Fatal(err)
			}
			execStartReject(t, f.verifier, valid, f.issuerWire, f.claims.Context, d, f.now)
		})
	}
	invalid := []struct {
		name   string
		change func(*ExecStartTicketClaims)
	}{
		{"version", func(c *ExecStartTicketClaims) { c.Version = 2 }}, {"purpose-renew", func(c *ExecStartTicketClaims) { c.Purpose = "operation_exec_renew" }}, {"zero-before", func(c *ExecStartTicketClaims) { c.NotBefore = time.Time{} }}, {"zero-after", func(c *ExecStartTicketClaims) { c.NotAfter = time.Time{} }}, {"equal-interval", func(c *ExecStartTicketClaims) { c.NotAfter = c.NotBefore }}, {"reversed", func(c *ExecStartTicketClaims) { c.NotAfter = c.NotBefore.Add(-time.Second) }}, {"over-30s", func(c *ExecStartTicketClaims) { c.NotAfter = c.NotBefore.Add(30*time.Second + time.Nanosecond) }}, {"business-expiry", func(c *ExecStartTicketClaims) { c.Context.ExpiresAt = c.NotAfter.Add(-time.Nanosecond) }}, {"before-issuer", func(c *ExecStartTicketClaims) {
			c.NotBefore = f.certificate.NotBefore.Add(-time.Nanosecond)
			c.NotAfter = c.NotBefore.Add(20 * time.Second)
		}}, {"after-issuer", func(c *ExecStartTicketClaims) {
			c.NotAfter = f.certificate.NotAfter.Add(time.Nanosecond)
			c.NotBefore = c.NotAfter.Add(-20 * time.Second)
			c.Context.ExpiresAt = c.NotAfter
		}}, {"bad-descriptor", func(c *ExecStartTicketClaims) { c.DescriptorDigest = "bad" }}, {"other-descriptor", func(c *ExecStartTicketClaims) { c.DescriptorDigest = strings.Repeat("c", 64) }},
	}
	invalid = append(invalid,
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"noncanonical-command", func(c *ExecStartTicketClaims) { c.Context.CommandID = strings.ToUpper(c.Context.CommandID) }},
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"nil-operation-uuid", func(c *ExecStartTicketClaims) { c.Context.OperationID = "00000000-0000-0000-0000-000000000000" }},
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"nil-issuer-uuid", func(c *ExecStartTicketClaims) { c.Context.IssuerCertificateID = "00000000-0000-0000-0000-000000000000" }},
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"request-segment", func(c *ExecStartTicketClaims) { c.Context.RequestID = "bad/request" }},
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"sandbox-segment", func(c *ExecStartTicketClaims) { c.Context.SandboxID = ".." }},
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"upper-operation-hash", func(c *ExecStartTicketClaims) { c.Context.OperationDigest = strings.ToUpper(c.Context.OperationDigest) }},
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"upper-workspace-hash", func(c *ExecStartTicketClaims) { c.Context.WorkspaceHash = strings.ToUpper(c.Context.WorkspaceHash) }},
		struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"invalid-runtime", func(c *ExecStartTicketClaims) { c.Context.Runtime.BootID = "boot\n" }},
	)
	// Zero/negative original counters must fail even when expected agrees with wire.
	for _, field := range []string{"Generation", "DataGateEpoch", "ControlRevision", "AdmissionRevision", "LeaseID"} {
		for _, value := range []int64{0, -1} {
			field, value := field, value
			invalid = append(invalid, struct {
				name   string
				change func(*ExecStartTicketClaims)
			}{fmt.Sprintf("invalid-%s-%d", field, value), func(c *ExecStartTicketClaims) { reflect.ValueOf(&c.Context).Elem().FieldByName(field).SetInt(value) }})
		}
	}
	for _, field := range []string{"Namespace", "AuthorityID", "Target", "RestoreEpoch", "IssuerCertificateID", "IssuerCertificateDigest", "CommandID", "OperationID", "RequestID", "OperationDigest", "SandboxID", "WorkspaceHash"} {
		field := field
		invalid = append(invalid, struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"empty-" + field, func(c *ExecStartTicketClaims) { reflect.ValueOf(&c.Context).Elem().FieldByName(field).SetString("") }})
	}
	for _, field := range []string{"ID", "UID", "BootID"} {
		field := field
		invalid = append(invalid, struct {
			name   string
			change func(*ExecStartTicketClaims)
		}{"empty-runtime-" + field, func(c *ExecStartTicketClaims) {
			reflect.ValueOf(&c.Context.Runtime).Elem().FieldByName(field).SetString("")
		}})
	}
	invalid = append(invalid, struct {
		name   string
		change func(*ExecStartTicketClaims)
	}{"zero-expiry", func(c *ExecStartTicketClaims) { c.Context.ExpiresAt = time.Time{} }})
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			c := f.claims
			tc.change(&c)
			if tc.name != "other-descriptor" {
				execStartSignReject(t, f.delegateKey, f.issuer, c)
			}
			execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, c, testExecStartDomain), f.issuerWire, c.Context, f.descriptor, f.now)
		})
	}
	for _, field := range []string{"NotBefore", "NotAfter", "ExpiresAt"} {
		t.Run("nonUTC-"+field, func(t *testing.T) {
			c := f.claims
			switch field {
			case "NotBefore":
				c.NotBefore = c.NotBefore.In(time.FixedZone("UTC-alias", 3600))
			case "NotAfter":
				c.NotAfter = c.NotAfter.In(time.FixedZone("UTC-alias", 3600))
			case "ExpiresAt":
				c.Context.ExpiresAt = c.Context.ExpiresAt.In(time.FixedZone("UTC-alias", 3600))
			}
			execStartSignReject(t, f.delegateKey, f.issuer, c)
			execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, c, testExecStartDomain), f.issuerWire, c.Context, f.descriptor, f.now)
		})
	}
	badKey := append(ed25519.PrivateKey(nil), f.delegateKey...)
	badKey[63] ^= 1
	for i, key := range []ed25519.PrivateKey{nil, f.delegateKey[:32], append(append(ed25519.PrivateKey(nil), f.delegateKey...), 0), badKey, f.rootKey} {
		t.Run(fmt.Sprintf("bad-private-%d", i), func(t *testing.T) { execStartSignReject(t, key, f.issuer, f.claims) })
	}
	execStartSignReject(t, f.delegateKey, CommandIssuerIdentity{}, f.claims)
	// Opaque identity consistency includes copied wire/digest/attributes, not just key length.
	for _, change := range []func(*CommandIssuerIdentity){func(i *CommandIssuerIdentity) { i.wire = nil }, func(i *CommandIssuerIdentity) { i.digest = "" }, func(i *CommandIssuerIdentity) { i.certificateID = "" }, func(i *CommandIssuerIdentity) { i.publicKey = nil }, func(i *CommandIssuerIdentity) { i.notBefore = time.Time{} }, func(i *CommandIssuerIdentity) { i.notAfter = time.Time{} }, func(i *CommandIssuerIdentity) { i.digest = strings.Repeat("c", 64) }, func(i *CommandIssuerIdentity) { i.certificateID = f.claims.Context.CommandID }, func(i *CommandIssuerIdentity) { i.notBefore = i.notBefore.Add(time.Second) }, func(i *CommandIssuerIdentity) {
		i.publicKey = append(ed25519.PublicKey(nil), i.publicKey...)
		i.publicKey[0] ^= 1
	}} {
		issuer := f.issuer
		change(&issuer)
		execStartSignReject(t, f.delegateKey, issuer, f.claims)
	}
	for _, v := range []*ManagementVerifier{nil, {}} {
		execStartReject(t, v, valid, f.issuerWire, f.claims.Context, f.descriptor, f.now)
	}
	execStartReject(t, f.verifier, valid, f.issuerWire, f.claims.Context, ExecutionDescriptor{}, f.now)
	for _, now := range []time.Time{{}, f.now.In(time.FixedZone("UTC-alias", 3600)), f.claims.NotBefore.Add(time.Second - time.Nanosecond), f.claims.NotAfter.Add(-time.Second), f.claims.Context.ExpiresAt} {
		execStartReject(t, f.verifier, valid, f.issuerWire, f.claims.Context, f.descriptor, now)
	}
	// Fresh issuer authentication cannot be replaced with an old in-memory identity.
	expired := f.certificate
	expired.NotAfter = f.now.Add(time.Second)
	expiredWire, err := SignCommandIssuerCertificate(f.rootKey, expired)
	if err != nil {
		t.Fatal(err)
	}
	c := f.claims
	c.Context.IssuerCertificateDigest = wireDigest(expiredWire)
	execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, c, testExecStartDomain), expiredWire, c.Context, f.descriptor, f.now)
	otherRoot, _ := managementTestKey(t)
	removed, err := NewManagementVerifier(managementTestBinding(f.certificate), []ed25519.PublicKey{otherRoot})
	if err != nil {
		t.Fatal(err)
	}
	execStartReject(t, removed, valid, f.issuerWire, f.claims.Context, f.descriptor, f.now)
	for _, issuerWire := range [][]byte{nil, append(append([]byte(nil), f.issuerWire...), bytes.Repeat([]byte(" "), 4097-len(f.issuerWire))...)} {
		execStartReject(t, f.verifier, valid, issuerWire, f.claims.Context, f.descriptor, f.now)
	}
	// Maximum 30s and business/issuer containment equality are allowed.
	for _, adjust := range []func(*ExecStartTicketClaims){func(c *ExecStartTicketClaims) { c.NotAfter = c.NotBefore.Add(30 * time.Second) }, func(c *ExecStartTicketClaims) { c.Context.ExpiresAt = c.NotAfter }, func(c *ExecStartTicketClaims) {
		c.NotBefore = f.certificate.NotBefore
		c.NotAfter = c.NotBefore.Add(30 * time.Second)
	}, func(c *ExecStartTicketClaims) {
		c.NotAfter = f.certificate.NotAfter
		c.NotBefore = c.NotAfter.Add(-30 * time.Second)
		c.Context.ExpiresAt = c.NotAfter
	}} {
		c := f.claims
		adjust(&c)
		w, err := SignExecStartTicket(f.delegateKey, f.issuer, c)
		if err != nil {
			t.Fatalf("valid interval boundary signing: %v", err)
		}
		if _, err := f.verifier.VerifyExecStartTicket(w, f.issuerWire, c.Context, f.descriptor, c.NotBefore.Add(2*time.Second)); err != nil {
			t.Fatalf("valid interval boundary verifying: %v", err)
		}
	}
	execStartStrictRejects(t, f, valid)
}

func execStartMutateJSON(t *testing.T, wire []byte, path []string, field string, null bool) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(wire, &object); err != nil {
		t.Fatal(err)
	}
	if len(path) > 0 {
		object[path[0]] = execStartMutateJSON(t, object[path[0]], path[1:], field, null)
	} else if null {
		object[field] = json.RawMessage(`null`)
	} else {
		delete(object, field)
	}
	out, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func execStartStrictRejects(t *testing.T, f execStartFixture, valid []byte) {
	for _, level := range []struct {
		path   []string
		fields []string
	}{
		{nil, []string{"claims", "signature"}}, {[]string{"claims"}, []string{"version", "purpose", "context", "descriptor_digest", "not_before", "not_after"}}, {[]string{"claims", "context"}, []string{"namespace", "authority_id", "target", "restore_epoch", "issuer_certificate_id", "issuer_certificate_digest", "command_id", "operation_id", "request_id", "operation_digest", "sandbox_id", "workspace_hash", "generation", "data_gate_epoch", "control_revision", "admission_revision", "lease_id", "runtime", "expires_at"}}, {[]string{"claims", "context", "runtime"}, []string{"id", "uid", "boot_id"}},
	} {
		for _, field := range level.fields {
			for _, null := range []bool{false, true} {
				t.Run(fmt.Sprintf("required-%s-%s-null-%t", strings.Join(level.path, "-"), field, null), func(t *testing.T) {
					execStartReject(t, f.verifier, execStartMutateJSON(t, valid, level.path, field, null), f.issuerWire, f.claims.Context, f.descriptor, f.now)
				})
			}
		}
	}
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"nil", func([]byte) []byte { return nil }}, {"oversize", func(w []byte) []byte { return append(w, bytes.Repeat([]byte(" "), 4097-len(w))...) }},
		{"unknown-envelope", func(w []byte) []byte { return append([]byte(`{"extra":1,`), w[1:]...) }}, {"duplicate-envelope", func(w []byte) []byte { return append([]byte(`{"signature":"AA==",`), w[1:]...) }},
		{"unknown-claims", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"version":`), []byte(`"extra":1,"version":`), 1)
		}}, {"duplicate-claims", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"version":`), []byte(`"version":1,"version":`), 1)
		}},
		{"unknown-context", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"namespace":`), []byte(`"extra":1,"namespace":`), 1)
		}}, {"duplicate-context", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"generation":`), []byte(`"generation":2,"generation":`), 1)
		}},
		{"unknown-runtime", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"boot_id":`), []byte(`"extra":1,"boot_id":`), 1)
		}}, {"duplicate-runtime", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"boot_id":`), []byte(`"boot_id":"boot-1","boot_id":`), 1)
		}},
		{"fraction", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"generation":2`), []byte(`"generation":2.0`), 1)
		}}, {"exponent", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"generation":2`), []byte(`"generation":2e0`), 1)
		}}, {"int64-overflow", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"generation":2`), []byte(`"generation":9223372036854775808`), 1)
		}}, {"uint32-overflow", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"version":1`), []byte(`"version":4294967296`), 1)
		}},
		{"invalid-utf8", func(w []byte) []byte { return bytes.Replace(w, []byte(`sandbox-1`), []byte{255}, 1) }}, {"surrogate", func(w []byte) []byte { return bytes.Replace(w, []byte(`sandbox-1`), []byte(`\ud800`), 1) }}, {"offset-time", func(w []byte) []byte { return bytes.Replace(w, []byte(`Z"`), []byte(`+00:00"`), 1) }}, {"trailing-json", func(w []byte) []byte { return append(w, []byte(`{}`)...) }}, {"malformed", func(w []byte) []byte { return w[:len(w)-1] }},
		{"bad-base64", func(w []byte) []byte {
			var e map[string]json.RawMessage
			json.Unmarshal(w, &e)
			e["signature"] = json.RawMessage(`"@invalid"`)
			out, _ := json.Marshal(e)
			return out
		}}, {"short-signature", func(w []byte) []byte {
			var e map[string]json.RawMessage
			json.Unmarshal(w, &e)
			e["signature"] = json.RawMessage(`"AA=="`)
			out, _ := json.Marshal(e)
			return out
		}}, {"long-signature", func(w []byte) []byte {
			var e execStartEnvelope
			json.Unmarshal(w, &e)
			e.Signature = append(e.Signature, 0)
			out, _ := json.Marshal(e)
			return out
		}}, {"bad-signature", func(w []byte) []byte {
			var e execStartEnvelope
			json.Unmarshal(w, &e)
			e.Signature[0] ^= 1
			out, _ := json.Marshal(e)
			return out
		}}, {"tamper-claims", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`request.not-a-uuid`), []byte(`request-other`), 1)
		}},
	} {
		t.Run("strict-"+tc.name, func(t *testing.T) {
			execStartReject(t, f.verifier, tc.change(append([]byte(nil), valid...)), f.issuerWire, f.claims.Context, f.descriptor, f.now)
		})
	}
	padded := append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), 4096-len(valid))...)
	if _, err := f.verifier.VerifyExecStartTicket(padded, f.issuerWire, f.claims.Context, f.descriptor, f.now); err != nil {
		t.Fatalf("4096B ticket rejected: %v", err)
	}
}

func TestExecStartRoleSeparation(t *testing.T) {
	f := execStartSetup(t)
	valid := execStartRaw(t, f.delegateKey, f.claims, testExecStartDomain)
	if _, err := f.verifier.VerifyExecStartTicket(valid, f.issuerWire, f.claims.Context, f.descriptor, f.now); err != nil {
		t.Fatalf("independent raw valid ticket: %v", err)
	}
	for _, domain := range []string{"sandbox-command-issuer-certificate:v1\x00", "sandbox-runtime-identity-certificate:v1\x00", "sandbox-runtime-certificate:v1\x00", "sandbox-runtime-ready:v1\x00", "sandbox-exec-descriptor:v1\x00", "sandbox-exec-start-ticket:v1\\x00", "sandbox-exec-start-ticket:v1", "sandbox-exec-renew-ticket:v1\x00"} {
		t.Run("domain-"+domain, func(t *testing.T) {
			execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, f.claims, domain), f.issuerWire, f.claims.Context, f.descriptor, f.now)
		})
	}
	_, otherDelegate := managementTestKey(t)
	for _, key := range []ed25519.PrivateKey{f.rootKey, otherDelegate} {
		execStartReject(t, f.verifier, execStartRaw(t, key, f.claims, testExecStartDomain), f.issuerWire, f.claims.Context, f.descriptor, f.now)
	}
	// Same cert UUID and root with another delegate is bound by both wire digest and key.
	replacement := f.certificate
	replacement.PublicKey = otherDelegate.Public().(ed25519.PublicKey)
	replacementWire, err := SignCommandIssuerCertificate(f.rootKey, replacement)
	if err != nil {
		t.Fatal(err)
	}
	execStartReject(t, f.verifier, valid, replacementWire, f.claims.Context, f.descriptor, f.now)
	replacementClaims := f.claims
	replacementClaims.Context.IssuerCertificateDigest = wireDigest(replacementWire)
	execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, replacementClaims, testExecStartDomain), replacementWire, replacementClaims.Context, f.descriptor, f.now)
	replacementClaims = f.claims
	replacementClaims.Purpose = "operation_exec_renew"
	execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, replacementClaims, testExecStartDomain), f.issuerWire, f.claims.Context, f.descriptor, f.now)
	// Valid signatures from all certificate roles cannot be used as start envelopes or issuer certs.
	receipt := RuntimeIdentityCertificateClaims{Version: 1, CertificateID: f.certificate.CertificateID, Role: "runtime_receipt", Namespace: f.certificate.Namespace, AuthorityID: f.certificate.AuthorityID, Target: f.certificate.Target, RestoreEpoch: f.certificate.RestoreEpoch, PublicKey: f.certificate.PublicKey, NotBefore: f.certificate.NotBefore, NotAfter: f.certificate.NotAfter, SandboxID: f.claims.Context.SandboxID, WorkspaceHash: f.claims.Context.WorkspaceHash, Generation: f.claims.Context.Generation, Runtime: f.claims.Context.Runtime}
	receiptWire, err := SignRuntimeIdentityCertificate(f.rootKey, receipt)
	if err != nil {
		t.Fatal(err)
	}
	birth := RuntimeCertificateClaims{Version: 1, Namespace: f.certificate.Namespace, AuthorityID: f.certificate.AuthorityID, Target: f.certificate.Target, RestoreEpoch: f.certificate.RestoreEpoch, IntentID: "intent", SandboxID: f.claims.Context.SandboxID, WorkspaceHash: f.claims.Context.WorkspaceHash, Generation: f.claims.Context.Generation, OperationID: f.claims.Context.OperationID, PayloadDigest: f.claims.Context.OperationDigest, Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("c", 64)}, ExpiresAt: f.certificate.NotAfter, Runtime: f.claims.Context.Runtime, WorkspaceMode: "plain", PublicKey: f.certificate.PublicKey, NotBefore: f.certificate.NotBefore, NotAfter: f.certificate.NotAfter}
	birthWire, err := SignRuntimeCertificate(f.rootKey, birth)
	if err != nil {
		t.Fatal(err)
	}
	readyWire, err := SignReadyReceipt(f.delegateKey, birthWire, ReadyReceiptClaims{Version: 1, CertificateDigest: wireDigest(birthWire), Claim: ClaimReference{ClaimID: f.claims.Context.CommandID, CreateRevision: 1, LeaseID: 1}, DataGateEpoch: 1, GateState: "open", ObservedAt: f.now.Add(-time.Second), ValidUntil: f.now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range [][]byte{f.issuerWire, receiptWire, birthWire, readyWire} {
		execStartReject(t, f.verifier, wire, f.issuerWire, f.claims.Context, f.descriptor, f.now)
		if !bytes.Equal(wire, f.issuerWire) {
			execStartReject(t, f.verifier, valid, wire, f.claims.Context, f.descriptor, f.now)
		}
	}
	wrongRole := f.certificate
	wrongRole.Role = "runtime_receipt"
	execStartReject(t, f.verifier, valid, managementRawWire(t, f.rootKey, wrongRole, "sandbox-command-issuer-certificate:v1\x00"), f.claims.Context, f.descriptor, f.now)
}

func TestExecStartBindingScope(t *testing.T) {
	f := execStartSetup(t)
	for _, tc := range execStartContextMutations()[:4] {
		t.Run(tc.name, func(t *testing.T) {
			c := f.claims
			tc.change(&c.Context)
			execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, c, testExecStartDomain), f.issuerWire, c.Context, f.descriptor, f.now)
		})
	}
}

func TestExecStartIssuerContainment(t *testing.T) {
	f := execStartSetup(t)
	cert := f.certificate
	cert.NotBefore = f.now.Add(-time.Second)
	cert.NotAfter = f.now.Add(5 * time.Second)
	issuerWire, err := SignCommandIssuerCertificate(f.rootKey, cert)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := f.verifier.VerifyCommandIssuerCertificate(issuerWire, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		before, after time.Time
	}{
		{"before-issuer", cert.NotBefore.Add(-time.Nanosecond), f.now.Add(3 * time.Second)},
		{"after-issuer", cert.NotBefore, cert.NotAfter.Add(time.Nanosecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := f.claims
			c.Context.IssuerCertificateDigest = issuer.Digest()
			c.NotBefore = tc.before
			c.NotAfter = tc.after
			execStartSignReject(t, f.delegateKey, issuer, c)
			execStartReject(t, f.verifier, execStartRaw(t, f.delegateKey, c, testExecStartDomain), issuerWire, c.Context, f.descriptor, f.now)
		})
	}
}

func TestExecStartSignIssuerBinding(t *testing.T) {
	f := execStartSetup(t)
	for _, tc := range execStartContextMutations()[4:6] {
		t.Run(tc.name, func(t *testing.T) {
			c := f.claims
			tc.change(&c.Context)
			execStartSignReject(t, f.delegateKey, f.issuer, c)
		})
	}
}

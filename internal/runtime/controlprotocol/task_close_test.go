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

	"github.com/stretchr/testify/require"
)

const taskTestTicketDomain = "sandbox-task-close-data-ticket:v1\x00"
const taskTestReceiptDomain = "sandbox-task-data-closed-receipt:v1\x00"

type taskCloseFixture struct {
	activationFixture
	issuer CommandIssuerIdentity
	ticket TaskCloseDataTicketClaims
}

func taskCloseSetup(t *testing.T) taskCloseFixture {
	t.Helper()
	f := newActivationFixture(t)
	issuer, e := f.verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.now)
	require.NoError(t, e)
	b, i := f.claims.Binding, f.claims.Identity
	c := TaskCloseDataContext{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: "8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskID: "9a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskDigest: strings.Repeat("b", 64), ClaimID: "aa1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", WorkerID: "worker-1", SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, DataGateEpoch: 3, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: i.Runtime}
	return taskCloseFixture{f, issuer, TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: c, NotBefore: f.now.Add(-2 * time.Second), NotAfter: f.now.Add(20 * time.Second)}}
}

// Independent standard-library signatures bypass every production signer and
// schema validator, so invalid signed claims test the verifier itself.
func taskCloseRaw(t *testing.T, key ed25519.PrivateKey, c any, domain string) []byte {
	t.Helper()
	payload, e := json.Marshal(c)
	require.NoError(t, e)
	wire, e := json.Marshal(struct {
		Claims    json.RawMessage `json:"claims"`
		Signature []byte          `json:"signature"`
	}{payload, ed25519.Sign(key, append([]byte(domain), payload...))})
	require.NoError(t, e)
	return wire
}
func taskTicketReject(t *testing.T, v *ManagementVerifier, w, issuer []byte, c TaskCloseDataContext, now time.Time) {
	t.Helper()
	e, err := v.VerifyTaskCloseDataTicket(w, issuer, c, now)
	require.Error(t, err)
	require.Equal(t, TaskCloseDataEvidence{}, e)
}
func taskTicketSignReject(t *testing.T, key ed25519.PrivateKey, i CommandIssuerIdentity, c TaskCloseDataTicketClaims) {
	t.Helper()
	w, e := SignTaskCloseDataTicket(key, i, c)
	require.Error(t, e)
	require.Nil(t, w)
}

func TestTaskCloseDataTicket(t *testing.T) {
	f := taskCloseSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	e, err := f.verifier.VerifyTaskCloseDataTicket(raw, f.issuerWire, f.ticket.Context, f.now)
	require.NoError(t, err)
	wire, err := SignTaskCloseDataTicket(f.issuerKey, f.issuer, f.ticket)
	require.NoError(t, err)
	require.Equal(t, raw, wire)
	sum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(sum[:]), e.Digest())
	require.Equal(t, f.ticket.Context, e.Context())
	require.Equal(t, f.ticket.NotBefore, e.NotBefore())
	require.Equal(t, f.ticket.NotAfter, e.NotAfter())
	pretty := new(bytes.Buffer)
	require.NoError(t, json.Indent(pretty, raw, "", " "))
	normalized, err := f.verifier.VerifyTaskCloseDataTicket(pretty.Bytes(), f.issuerWire, f.ticket.Context, f.now)
	require.NoError(t, err)
	require.Equal(t, e.Digest(), normalized.Digest())
	require.Equal(t, raw, normalized.Wire())
	for _, now := range []time.Time{f.ticket.NotBefore.Add(time.Second), f.ticket.NotAfter.Add(-time.Second - time.Nanosecond)} {
		_, err = f.verifier.VerifyTaskCloseDataTicket(raw, f.issuerWire, f.ticket.Context, now)
		require.NoError(t, err)
	}
	c := f.ticket
	c.NotAfter = c.NotBefore.Add(30 * time.Second)
	_, err = SignTaskCloseDataTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
}
func TestTaskCloseDataTicketRejects(t *testing.T) {
	f := taskCloseSetup(t)
	valid := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	for name, mutate := range taskContextChanges() {
		t.Run("context-"+name, func(t *testing.T) {
			c := f.ticket
			mutate(&c.Context)
			taskTicketReject(t, f.verifier, valid, f.issuerWire, c.Context, f.now)
			taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), f.issuerWire, f.ticket.Context, f.now)
		})
	}
	for name, mutate := range taskInvalidContexts() {
		t.Run("invalid-"+name, func(t *testing.T) {
			c := f.ticket
			mutate(&c.Context)
			taskTicketSignReject(t, f.issuerKey, f.issuer, c)
			taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), f.issuerWire, c.Context, f.now)
		})
	}
	for name, times := range taskBadTimes(f.ticket.NotBefore, f.ticket.NotAfter) {
		t.Run(name, func(t *testing.T) {
			c := f.ticket
			c.NotBefore, c.NotAfter = times[0], times[1]
			taskTicketSignReject(t, f.issuerKey, f.issuer, c)
			taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), f.issuerWire, c.Context, f.now)
		})
	}
	for _, purpose := range []string{"", "operation_exec_start", "data_closed"} {
		c := f.ticket
		c.Purpose = purpose
		taskTicketSignReject(t, f.issuerKey, f.issuer, c)
		taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), f.issuerWire, c.Context, f.now)
	}
	c := f.ticket
	c.Version = 2
	taskTicketSignReject(t, f.issuerKey, f.issuer, c)
	taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), f.issuerWire, c.Context, f.now)
	for _, field := range []string{"Namespace", "AuthorityID", "Target", "RestoreEpoch"} {
		t.Run("signer-binding-"+field, func(t *testing.T) {
			c := f.ticket
			taskContextChanges()[field](&c.Context)
			taskTicketSignReject(t, f.issuerKey, f.issuer, c)
			taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), f.issuerWire, c.Context, f.now)
		})
	}
	for _, window := range [][2]time.Time{{f.issuer.NotBefore().Add(-time.Nanosecond), f.issuer.NotBefore().Add(time.Second)}, {f.issuer.NotAfter().Add(-time.Second), f.issuer.NotAfter().Add(time.Nanosecond)}} {
		c := f.ticket
		c.NotBefore, c.NotAfter = window[0], window[1]
		taskTicketSignReject(t, f.issuerKey, f.issuer, c)
		taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), f.issuerWire, c.Context, f.now)
	}
	corruptKey := bytes.Clone(f.issuerKey)
	corruptKey[63] ^= 1
	for _, key := range []ed25519.PrivateKey{nil, f.issuerKey[:32], append(bytes.Clone(f.issuerKey), 0), corruptKey, f.root, f.runtimeKey} {
		taskTicketSignReject(t, key, f.issuer, f.ticket)
	}
	taskTicketSignReject(t, f.issuerKey, CommandIssuerIdentity{}, f.ticket)
	for _, mutate := range []func(*CommandIssuerIdentity){func(i *CommandIssuerIdentity) { i.wire = nil }, func(i *CommandIssuerIdentity) { i.digest = "" }, func(i *CommandIssuerIdentity) { i.publicKey = nil }, func(i *CommandIssuerIdentity) { i.certificateID = "" }, func(i *CommandIssuerIdentity) { i.notBefore = i.notBefore.Add(time.Second) }} {
		i := f.issuer
		mutate(&i)
		taskTicketSignReject(t, f.issuerKey, i, f.ticket)
	}
	otherRoot, _ := managementTestKey(t)
	v, e := NewManagementVerifier(f.claims.Binding, []ed25519.PublicKey{otherRoot})
	require.NoError(t, e)
	taskTicketReject(t, v, valid, f.issuerWire, f.ticket.Context, f.now)
	taskTicketReject(t, f.verifier, valid, f.runtimeWire, f.ticket.Context, f.now)
	var cert commandIssuerEnvelope
	require.NoError(t, json.Unmarshal(f.issuerWire, &cert))
	badCert := cert.Claims
	badCert.Role = "runtime_receipt"
	taskTicketReject(t, f.verifier, valid, managementRawWire(t, f.root, badCert, "sandbox-command-issuer-certificate:v1\x00"), f.ticket.Context, f.now)
	cert.Claims.NotAfter = f.now.Add(time.Second)
	iw, e := SignCommandIssuerCertificate(f.root, cert.Claims)
	require.NoError(t, e)
	c = f.ticket
	c.Context.IssuerCertificateDigest = wireDigest(iw)
	taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain), iw, c.Context, f.now)
	taskTicketReject(t, f.verifier, taskCloseRaw(t, f.runtimeKey, f.ticket, taskTestTicketDomain), f.issuerWire, f.ticket.Context, f.now)
	f = taskCloseSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	for _, domain := range []string{"sandbox-task-close-data-ticket:v1", `sandbox-task-close-data-ticket:v1\x00`, taskTestReceiptDomain, "sandbox-exec-start-ticket:v1\x00"} {
		taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, f.ticket, domain), f.issuerWire, f.ticket.Context, f.now)
	}
	for _, now := range []time.Time{{}, f.now.In(time.FixedZone("offset", 3600)), f.ticket.NotBefore.Add(time.Second - time.Nanosecond), f.ticket.NotAfter.Add(-time.Second)} {
		taskTicketReject(t, f.verifier, raw, f.issuerWire, f.ticket.Context, now)
	}
	for _, v := range []*ManagementVerifier{nil, {}} {
		taskTicketReject(t, v, raw, f.issuerWire, f.ticket.Context, f.now)
	}
}
func TestTaskCloseDataTicketStrict(t *testing.T) {
	t.Run("signer-actual-wire-cap", func(t *testing.T) {
		f := taskCloseSetup(t)
		var cert commandIssuerEnvelope
		require.NoError(t, json.Unmarshal(f.issuerWire, &cert))
		cert.Claims.AuthorityID = strings.Repeat("a", 128)
		cert.Claims.Target = strings.Repeat("t", 128)
		cert.Claims.RestoreEpoch = strings.Repeat("r", 128)
		cert.Claims.Namespace = "/" + strings.Repeat("n", 250) + "/" + strings.Repeat("n", 128) + "/" + strings.Repeat("n", 128) + "/"
		iw, err := SignCommandIssuerCertificate(f.root, cert.Claims)
		require.NoError(t, err)
		binding := managementTestBinding(cert.Claims)
		v, err := NewManagementVerifier(binding, []ed25519.PublicKey{ed25519.PublicKey(f.root[32:])})
		require.NoError(t, err)
		issuer, err := v.VerifyCommandIssuerCertificate(iw, f.now)
		require.NoError(t, err)
		c := f.ticket
		c.Context.Namespace = binding.Namespace
		c.Context.AuthorityID = binding.AuthorityID
		c.Context.Target = binding.Target
		c.Context.RestoreEpoch = binding.RestoreEpoch
		c.Context.IssuerCertificateDigest = issuer.Digest()
		c.Context.WorkerID = strings.Repeat("w", 128)
		c.Context.SandboxID = strings.Repeat("s", 128)
		c.Context.Runtime = RuntimeReference{ID: strings.Repeat("<", 128), UID: strings.Repeat("<", 128), BootID: strings.Repeat("<", 128)}
		raw := taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain)
		require.Greater(t, len(raw), 4096)
		taskTicketSignReject(t, f.issuerKey, issuer, c)
		taskTicketReject(t, v, raw, iw, c.Context, f.now)
	})

	f := taskCloseSetup(t)
	valid := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	taskStrictCases(t, valid, func(t *testing.T, w []byte) {
		taskTicketReject(t, f.verifier, w, f.issuerWire, f.ticket.Context, f.now)
	})
	taskTicketReject(t, f.verifier, runtimeIdentityMutate(t, valid, []string{"claims", "context", "expires_at"}, json.RawMessage(`"2000-01-01T00:00:00Z"`), false), f.issuerWire, f.ticket.Context, f.now)
	c := f.ticket
	c.Context.Runtime.UID = "replacement-�"
	w := taskCloseRaw(t, f.issuerKey, c, taskTestTicketDomain)
	_, err := f.verifier.VerifyTaskCloseDataTicket(w, f.issuerWire, c.Context, f.now)
	require.NoError(t, err)
	for _, bad := range [][]byte{[]byte(`\ud800`), []byte(`\udc00`), {255}} {
		taskTicketReject(t, f.verifier, bytes.Replace(w, []byte("�"), bad, 1), f.issuerWire, c.Context, f.now)
	}
	f = taskCloseSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	padded := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), 4096-len(raw))...)
	e, err := f.verifier.VerifyTaskCloseDataTicket(padded, f.issuerWire, f.ticket.Context, f.now)
	require.NoError(t, err)
	require.Equal(t, raw, e.Wire())
	taskTicketReject(t, f.verifier, append(padded, ' '), f.issuerWire, f.ticket.Context, f.now)
}
func TestTaskCloseDataTicketCopies(t *testing.T) {
	f := taskCloseSetup(t)
	w, err := SignTaskCloseDataTicket(f.issuerKey, f.issuer, f.ticket)
	require.NoError(t, err)
	original := bytes.Clone(w)
	e, err := f.verifier.VerifyTaskCloseDataTicket(w, f.issuerWire, f.ticket.Context, f.now)
	require.NoError(t, err)
	clear(w)
	clear(f.issuerKey)
	clear(f.issuerWire)
	clear(f.issuer.PublicKey())
	clear(f.issuer.Wire())
	clear(e.Wire())
	c := e.Context()
	c.Runtime.BootID = "changed"
	require.Equal(t, original, e.Wire())
	require.Equal(t, f.ticket.Context, e.Context())
	var wg sync.WaitGroup
	for j := 0; j < 4; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				clear(e.Wire())
				if !bytes.Equal(original, e.Wire()) {
					t.Error("evidence wire aliased")
				}
			}
		}()
	}
	wg.Wait()
}

func taskContextChanges() map[string]func(*TaskCloseDataContext) {
	changes := map[string]func(*TaskCloseDataContext){}
	typ := reflect.TypeOf(TaskCloseDataContext{})
	for j := 0; j < typ.NumField(); j++ {
		field := typ.Field(j).Name
		if field == "Runtime" {
			continue
		}
		changes[field] = func(c *TaskCloseDataContext) {
			v := reflect.ValueOf(c).Elem().FieldByName(field)
			if v.Kind() == reflect.Int64 {
				if field == "ControlRevision" {
					v.SetInt(v.Int() - 1)
				} else {
					v.SetInt(v.Int() + 1)
				}
				return
			}
			switch field {
			case "Namespace":
				v.SetString("/sandbox/control/other/")
			case "IssuerCertificateID", "CommandID", "TaskID", "ClaimID":
				v.SetString("ba1c1595-cbfd-4d2e-9ef7-f4dd3c84b172")
			case "IssuerCertificateDigest", "TaskDigest", "WorkspaceHash":
				v.SetString(strings.Repeat("f", 64))
			default:
				v.SetString("other")
			}
		}
	}
	for _, field := range []string{"ID", "UID", "BootID"} {
		changes["Runtime-"+field] = func(c *TaskCloseDataContext) {
			reflect.ValueOf(&c.Runtime).Elem().FieldByName(field).SetString("other")
		}
	}
	return changes
}
func taskInvalidContexts() map[string]func(*TaskCloseDataContext) {
	changes := map[string]func(*TaskCloseDataContext){"claim-before-control": func(c *TaskCloseDataContext) { c.ClaimCreateRevision = c.ControlRevision - 1 }, "claim-at-control": func(c *TaskCloseDataContext) { c.ClaimCreateRevision = c.ControlRevision }, "worker-path": func(c *TaskCloseDataContext) { c.WorkerID = "bad/worker" }, "worker-limit": func(c *TaskCloseDataContext) { c.WorkerID = strings.Repeat("x", 129) }, "runtime-control": func(c *TaskCloseDataContext) { c.Runtime.BootID = "bad\n" }}
	typ := reflect.TypeOf(TaskCloseDataContext{})
	for j := 0; j < typ.NumField(); j++ {
		field := typ.Field(j).Name
		if field == "Runtime" {
			continue
		}
		if typ.Field(j).Type.Kind() == reflect.String {
			changes["empty-"+field] = func(c *TaskCloseDataContext) { reflect.ValueOf(c).Elem().FieldByName(field).SetString("") }
		} else {
			for _, n := range []int64{0, -1} {
				changes[fmt.Sprintf("%s-%d", field, n)] = func(c *TaskCloseDataContext) { reflect.ValueOf(c).Elem().FieldByName(field).SetInt(n) }
			}
		}
	}
	for _, field := range []string{"IssuerCertificateID", "CommandID", "TaskID", "ClaimID"} {
		changes["zero-uuid-"+field] = func(c *TaskCloseDataContext) {
			reflect.ValueOf(c).Elem().FieldByName(field).SetString("00000000-0000-0000-0000-000000000000")
		}
		changes["upper-uuid-"+field] = func(c *TaskCloseDataContext) {
			v := reflect.ValueOf(c).Elem().FieldByName(field)
			v.SetString(strings.ToUpper(v.String()))
		}
	}
	for _, field := range []string{"IssuerCertificateDigest", "TaskDigest", "WorkspaceHash"} {
		changes["upper-hash-"+field] = func(c *TaskCloseDataContext) {
			reflect.ValueOf(c).Elem().FieldByName(field).SetString(strings.Repeat("A", 64))
		}
	}
	for _, field := range []string{"ID", "UID", "BootID"} {
		changes["empty-runtime-"+field] = func(c *TaskCloseDataContext) { reflect.ValueOf(&c.Runtime).Elem().FieldByName(field).SetString("") }
	}
	return changes
}
func taskBadTimes(before, after time.Time) map[string][2]time.Time {
	return map[string][2]time.Time{"zero-before": {{}, after}, "zero-after": {before, {}}, "equal": {before, before}, "reversed": {after, before}, "over30": {before, before.Add(30*time.Second + time.Nanosecond)}, "offset-before": {before.In(time.FixedZone("offset", 3600)), after}, "offset-after": {before, after.In(time.FixedZone("offset", 3600))}}
}

// Reject recursively omitted/null/duplicate/unknown fields without relying on
// production schemas. Duplicate/unknown mutations preserve valid signed claims.
func taskStrictCases(t *testing.T, valid []byte, reject func(*testing.T, []byte)) {
	t.Helper()
	var walk func([]byte, []string)
	walk = func(object []byte, path []string) {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(object, &fields))
		for field, value := range fields {
			full := append(append([]string(nil), path...), field)
			for _, null := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/null=%t", strings.Join(full, "/"), null), func(t *testing.T) { reject(t, runtimeIdentityMutate(t, valid, full, json.RawMessage("null"), !null)) })
			}
			key, _ := json.Marshal(field)
			duplicate := append(append(append(append([]byte{'{'}, key...), ':'), value...), ',')
			duplicate = append(duplicate, object[1:]...)
			w := duplicate
			if len(path) > 0 {
				w = runtimeIdentityMutate(t, valid, path, duplicate, false)
			}
			t.Run(strings.Join(full, "/")+"/duplicate", func(t *testing.T) { reject(t, w) })
			if len(value) > 0 && value[0] == '{' {
				walk(value, full)
			}
		}
		unknown := append([]byte(`{"unknown":true,`), object[1:]...)
		w := unknown
		if len(path) > 0 {
			w = runtimeIdentityMutate(t, valid, path, unknown, false)
		}
		t.Run(strings.Join(path, "/")+"/unknown", func(t *testing.T) { reject(t, w) })
	}
	walk(valid, nil)
	var original map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(valid, &original))
	var signature []byte
	require.NoError(t, json.Unmarshal(original["signature"], &signature))
	for name, sig := range map[string][]byte{"long-signature": append(bytes.Clone(signature), 0), "wrong-signature": append([]byte{signature[0] ^ 1}, signature[1:]...)} {
		b, e := json.Marshal(sig)
		require.NoError(t, e)
		t.Run(name, func(t *testing.T) { reject(t, runtimeIdentityMutate(t, valid, []string{"signature"}, b, false)) })
	}

	for name, w := range map[string][]byte{"nil": nil, "truncated": valid[:len(valid)-1], "trailing": append(bytes.Clone(valid), []byte("{}")...), "nonutf8": append(bytes.Clone(valid), 255), "offset": bytes.Replace(valid, []byte(`Z"`), []byte(`+00:00"`), 1), "fraction": bytes.Replace(valid, []byte(`"generation":7`), []byte(`"generation":7.0`), 1), "exponent": bytes.Replace(valid, []byte(`"generation":7`), []byte(`"generation":7e0`), 1), "overflow": bytes.Replace(valid, []byte(`"lease_id":12`), []byte(`"lease_id":9223372036854775808`), 1), "version-overflow": bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":4294967296`), 1), "signature-short": runtimeIdentityMutate(t, valid, []string{"signature"}, json.RawMessage(`"AA=="`), false), "signature-base64": runtimeIdentityMutate(t, valid, []string{"signature"}, json.RawMessage(`"!"`), false)} {
		t.Run(name, func(t *testing.T) { reject(t, w) })
	}
}

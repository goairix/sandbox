package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestTaskQuiesceTicketStrict(t *testing.T) {
	f, c := quiesceSetup(t)
	w := taskCloseRaw(t, f.issuerKey, c, "sandbox-task-quiesce-users-ticket:v1\x00")
	reject := func(t *testing.T, b []byte) {
		e, err := f.verifier.VerifyTaskUserQuiescenceTicket(b, f.issuerWire, c.Context, f.now)
		require.Error(t, err)
		require.Equal(t, TaskUserQuiescenceEvidence{}, e)
	}
	taskStrictCases(t, w, reject)
	padded := append(bytes.Clone(w), bytes.Repeat([]byte(" "), 8192-len(w))...)
	e, err := f.verifier.VerifyTaskUserQuiescenceTicket(padded, f.issuerWire, c.Context, f.now)
	require.NoError(t, err)
	require.Equal(t, w, e.Wire())
	reject(t, append(padded, ' '))
	for name, mutate := range taskInvalidContexts() {
		t.Run("invalid-current-"+name, func(t *testing.T) {
			bad := c
			mutate(&bad.Context.Current)
			_, err := SignTaskUserQuiescenceTicket(f.issuerKey, f.issuer, bad)
			require.Error(t, err)
			reject(t, taskCloseRaw(t, f.issuerKey, bad, "sandbox-task-quiesce-users-ticket:v1\x00"))
		})
	}
	for name, interval := range taskBadTimes(c.NotBefore, c.NotAfter) {
		t.Run(name, func(t *testing.T) {
			bad := c
			bad.NotBefore, bad.NotAfter = interval[0], interval[1]
			_, err := SignTaskUserQuiescenceTicket(f.issuerKey, f.issuer, bad)
			require.Error(t, err)
			reject(t, taskCloseRaw(t, f.issuerKey, bad, "sandbox-task-quiesce-users-ticket:v1\x00"))
		})
	}
	var cert commandIssuerEnvelope
	require.NoError(t, json.Unmarshal(f.issuerWire, &cert))
	for name, m := range map[string]func(*CommandIssuerCertificateClaims){"binding": func(x *CommandIssuerCertificateClaims) { x.Target = "other" }, "expired": func(x *CommandIssuerCertificateClaims) { x.NotAfter = f.now.Add(time.Second) }, "interval": func(x *CommandIssuerCertificateClaims) { x.NotBefore = c.NotBefore.Add(time.Nanosecond) }} {
		t.Run("issuer-"+name, func(t *testing.T) {
			x := cert.Claims
			m(&x)
			iw, err := SignCommandIssuerCertificate(f.root, x)
			require.NoError(t, err)
			_, err = f.verifier.VerifyTaskUserQuiescenceTicket(w, iw, c.Context, f.now)
			require.Error(t, err)
		})
	}
	// Independent valid issuer plus a structurally valid different context binding
	// must be refused by both signer and verifier, not by context parsing.
	bad := c
	bad.Context.Current.Target = "other"
	bad.Context.CloseDataContext.Target = "other"
	require.NoError(t, validateTaskQuiescenceContext(bad.Context))
	_, err = SignTaskUserQuiescenceTicket(f.issuerKey, f.issuer, bad)
	require.ErrorContains(t, err, "binding")
	var nilVerifier *ManagementVerifier
	_, err = nilVerifier.VerifyTaskUserQuiescenceTicket(w, f.issuerWire, c.Context, f.now)
	require.Error(t, err)
}

func TestTaskQuiesceReceiptStrict(t *testing.T) {
	f, c := quiesceSetup(t)
	d := strings.Repeat("e", 64)
	a := TaskUserQuiescenceAcceptedClaims{Version: 1, State: "quiescence_accepted", Context: c.Context, TicketDigest: d, NotBefore: c.NotBefore, NotAfter: c.NotAfter}
	r := TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: c.Context, TicketDigest: d, NotBefore: c.NotBefore, NotAfter: c.NotAfter, ExecutionSetDigest: wireDigest([]byte("[]"))}
	for _, terminal := range []bool{false, true} {
		name := "accepted"
		domain := "sandbox-task-quiesce-users-accepted:v1\x00"
		var claims any = a
		if terminal {
			name = "terminal"
			domain = "sandbox-task-users-quiesced-receipt:v1\x00"
			claims = r
		}
		t.Run(name, func(t *testing.T) {
			w := taskCloseRaw(t, f.runtimeKey, claims, domain)
			verify := func(w, cert []byte, expected TaskUserQuiescenceContext, now time.Time) error {
				if terminal {
					e, err := f.verifier.VerifyTaskUserQuiescenceReceipt(w, cert, expected, d, c.NotBefore, c.NotAfter, now)
					if err != nil {
						require.Equal(t, TaskUserQuiescenceReceiptEvidence{}, e)
					}
					return err
				}
				e, err := f.verifier.VerifyTaskUserQuiescenceAccepted(w, cert, expected, d, c.NotBefore, c.NotAfter, now)
				if err != nil {
					require.Equal(t, TaskUserQuiescenceAcceptedEvidence{}, e)
				}
				return err
			}
			reject := func(t *testing.T, w []byte) { require.Error(t, verify(w, f.runtimeWire, c.Context, f.now)) }
			taskStrictCases(t, w, reject)
			padded := append(bytes.Clone(w), bytes.Repeat([]byte(" "), 12288-len(w))...)
			require.NoError(t, verify(padded, f.runtimeWire, c.Context, f.now))
			reject(t, append(padded, ' '))
			require.NoError(t, verify(w, f.runtimeWire, c.Context, f.now.Add(25*time.Second)))
			for _, domain := range []string{strings.TrimSuffix(domain, "\x00"), "sandbox-task-data-closed-receipt:v1\x00"} {
				reject(t, taskCloseRaw(t, f.runtimeKey, claims, domain))
			}
			reject(t, taskCloseRaw(t, f.issuerKey, claims, domain))
			for field, mutate := range taskContextChanges() {
				t.Run("current-"+field, func(t *testing.T) {
					bad := c.Context
					mutate(&bad.Current)
					if bad == c.Context {
						bad.Current.CommandID = "ca1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
					}
					require.Error(t, verify(w, f.runtimeWire, bad, f.now))
				})
				t.Run("history-"+field, func(t *testing.T) {
					bad := c.Context
					mutate(&bad.CloseDataContext)
					require.Error(t, verify(w, f.runtimeWire, bad, f.now))
				})
			}
			var cert runtimeIdentityEnvelope
			require.NoError(t, json.Unmarshal(f.runtimeWire, &cert))
			for name, mutate := range map[string]func(*RuntimeIdentityCertificateClaims){"expired": func(x *RuntimeIdentityCertificateClaims) { x.NotAfter = f.now.Add(time.Second) }, "interval-before": func(x *RuntimeIdentityCertificateClaims) { x.NotBefore = c.NotBefore.Add(time.Nanosecond) }, "interval-after": func(x *RuntimeIdentityCertificateClaims) { x.NotAfter = c.NotAfter.Add(-time.Nanosecond) }, "boot": func(x *RuntimeIdentityCertificateClaims) { x.Runtime.BootID = "other" }, "binding": func(x *RuntimeIdentityCertificateClaims) { x.Target = "other" }} {
				t.Run("runtime-"+name, func(t *testing.T) {
					x := cert.Claims
					mutate(&x)
					cw, err := SignRuntimeIdentityCertificate(f.root, x)
					require.NoError(t, err)
					require.Error(t, verify(w, cw, c.Context, f.now))
				})
			}
			for _, now := range []time.Time{{}, f.now.In(time.FixedZone("bad", 3600)), f.now.Add(2 * time.Hour)} {
				require.Error(t, verify(w, f.runtimeWire, c.Context, now))
			}
			for _, key := range []ed25519.PrivateKey{nil, f.runtimeKey[:32]} {
				var err error
				if terminal {
					_, err = SignTaskUserQuiescenceReceipt(key, r)
				} else {
					_, err = SignTaskUserQuiescenceAccepted(key, a)
				}
				require.Error(t, err)
			}
		})
	}
}

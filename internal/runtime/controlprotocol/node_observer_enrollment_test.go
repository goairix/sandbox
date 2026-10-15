package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeObserverProtocolEnrollment(t *testing.T) {
	f := newActivationFixture(t)
	a, err := SignTargetActivation(f.root, f.claims)
	require.NoError(t, err)
	r := NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", RuntimeCertificate: f.runtimeWire, Activation: a, IssuerCertificate: f.issuerWire}
	w, err := EncodeNodeEnrollmentRequest(r)
	require.NoError(t, err)
	got, err := DecodeNodeEnrollmentRequest(w)
	require.NoError(t, err)
	e, err := f.verifier.VerifyNodeEnrollmentRequest(got, f.now)
	require.NoError(t, err)
	require.Equal(t, f.birth, e.Birth())
	require.Equal(t, f.claims.Identity, e.Identity())
	for _, bad := range [][]byte{append([]byte(" "), w...), bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(w, []byte(`"purpose":"runtime_node_enrollment"`), []byte(`"purpose":null`), 1), bytes.Repeat([]byte(" "), 16385)} {
		_, err := DecodeNodeEnrollmentRequest(bad)
		require.Error(t, err)
	}
	for name, change := range map[string]func(*NodeEnrollmentRequest){
		"activation-signature": func(r *NodeEnrollmentRequest) {
			r.Activation = bytes.Replace(r.Activation, []byte(`"uid":1000`), []byte(`"uid":1001`), 1)
		},
		"issuer-role":  func(r *NodeEnrollmentRequest) { r.IssuerCertificate = r.RuntimeCertificate },
		"runtime-role": func(r *NodeEnrollmentRequest) { r.RuntimeCertificate = r.IssuerCertificate },
	} {
		t.Run(name, func(t *testing.T) {
			bad := r
			change(&bad)
			_, err := f.verifier.VerifyNodeEnrollmentRequest(bad, f.now)
			require.Error(t, err)
		})
	}
	runtime, err := f.verifier.VerifyRuntimeIdentityCertificate(f.runtimeWire, e.Identity(), f.now)
	require.NoError(t, err)
	_, _, cert, observer := nodeObserverCertificateFixture(t)
	c := NodeEnrollmentChallenge{Version: 1, Purpose: "runtime_node_enrollment_challenge", ObserverCertificate: observer, Nonce: strings.Repeat("a", 64), RequestDigest: wireDigest(w), Runtime: e.Identity().Runtime, NodeUID: cert.NodeUID, NodeBootID: cert.NodeBootID}
	signed, err := NodeEnrollmentChallengeSigningBytes(c)
	require.NoError(t, err)
	response := NodeEnrollmentResponse{Version: 1, Purpose: "runtime_node_enrollment_response", Signature: ed25519.Sign(f.runtimeKey, signed)}
	require.NoError(t, VerifyNodeEnrollmentResponse(c, response, runtime))
	c.Nonce = strings.Repeat("b", 64)
	require.Error(t, VerifyNodeEnrollmentResponse(c, response, runtime))
	c.Nonce = strings.Repeat("a", 64)
	c.RequestDigest = strings.Repeat("c", 64)
	require.Error(t, VerifyNodeEnrollmentResponse(c, response, runtime))
}

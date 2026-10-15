package controlprotocol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeObserverProtocolReceipts(t *testing.T) {
	f, key, cert, wire := nodeObserverCertificateFixture(t)
	identity, err := f.verifier.VerifyNodeObserverCertificate(wire, cert.NodeUID, cert.NodeBootID, f.now)
	require.NoError(t, err)
	c := NodeEnrollmentReceiptClaims{Version: 1, Purpose: "runtime_node_enrolled", EnrollmentDigest: strings.Repeat("a", 64), ObserverCertificateDigest: identity.Digest(), NodeUID: cert.NodeUID, NodeBootID: cert.NodeBootID, ActivationDigest: strings.Repeat("b", 64), Runtime: f.claims.Identity.Runtime, SandboxID: f.claims.Identity.SandboxID, WorkspaceHash: f.claims.Identity.WorkspaceHash, Generation: f.claims.Identity.Generation, CreationIntentID: "ca1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", ObservedAt: f.now}
	r, err := SignNodeEnrollmentReceipt(key, identity, c)
	require.NoError(t, err)
	e, err := f.verifier.VerifyNodeEnrollmentReceipt(r.Wire(), wire, c, f.now)
	require.NoError(t, err)
	require.Equal(t, c, e.Claims)
	c.EnrollmentDigest = strings.Repeat("c", 64)
	_, err = f.verifier.VerifyNodeEnrollmentReceipt(r.Wire(), wire, c, f.now)
	require.Error(t, err)
	q := NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: 0xaa, TaskID: f.ticket.Context.TaskID, CommandID: f.ticket.Context.CommandID, IntentCreateRevision: 14, IntentDigest: strings.Repeat("d", 64), EnrollmentDigest: strings.Repeat("a", 64)}
	_, err = EncodeNodeTerminationQuery(q)
	require.NoError(t, err)
	q.Purpose = "task_runtime_terminate"
	_, err = EncodeNodeTerminationQuery(q)
	require.Error(t, err)
}

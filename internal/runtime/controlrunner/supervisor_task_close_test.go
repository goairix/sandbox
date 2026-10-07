package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	tpt "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
)

// This private consumer test uses real persisted passive history, not a forged
// PID1/live activation. Closing admission must not sign unknown/accepted state.
func TestTaskCloseQueryRefusesNonterminalHistory(t *testing.T) {
	journal, id := shutdownHistory(t)
	r, err := journal.Lookup(context.Background(), id)
	require.NoError(t, err)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s := &Supervisor{journal: journal, key: key}
	_, wire, err := s.queryReceipt(context.Background(), tpt.Envelope{Purpose: "exec_query", Context: r.Context, DescriptorDigest: r.DescriptorDigest})
	require.Error(t, err)
	require.Nil(t, wire)
}

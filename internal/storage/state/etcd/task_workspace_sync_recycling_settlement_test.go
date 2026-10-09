package etcd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskWorkspaceSyncSettlementOriginalAuthority(t *testing.T) {
	var source *PreparedTaskWorkspaceSyncSource
	_, err := source.settleLocal(context.Background(), nil)
	require.Error(t, err)
	_, err = source.recycleLocal(context.Background(), nil)
	require.Error(t, err)
	d := &taskWorkspaceSyncIntentDraft{}
	p := &taskWorkspaceSyncLocalPut{draft: d}
	p.self, d.localPut = p, p
	w := &taskWorkspaceSyncLocalWitness{origin: p}
	w.self, p.witness = w, w
	q := &taskWorkspaceSyncLocalSettlement{witness: w, draft: d}
	q.self = q
	require.False(t, q.original(), "self and witness alone cannot grant GC")
	d.localSettlement = q
	require.True(t, q.original())
	copy := *q
	copy.self = &copy
	require.False(t, copy.original(), "reset copied self cannot replace the original settlement registration")
	witnessCopy := *w
	putCopy := &taskWorkspaceSyncLocalPut{draft: p.draft, witness: &witnessCopy}
	putCopy.self = putCopy
	witnessCopy.self, witnessCopy.origin = &witnessCopy, putCopy
	copy.witness = &witnessCopy
	require.False(t, copy.original(), "joint copies cannot reconstruct GC authority")
}

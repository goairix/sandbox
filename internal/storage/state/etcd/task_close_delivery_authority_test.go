package etcd

import (
	"context"
	"crypto/ed25519"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTaskCloseDeliveryQueryOnlyAuthority(t *testing.T) {
	o := validOptions(t)
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
	o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("query-only construction observed clock") })
	v, err := taskAuthority(o)
	require.NoError(t, err)
	require.NotNil(t, v)
	b := &Backend{taskVerifier: v}
	r, err := b.PrepareTaskCloseData(context.Background(), &TaskClaim{})
	require.Error(t, err)
	require.Nil(t, r.Prepared)
}

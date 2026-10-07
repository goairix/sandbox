package etcd

import (
	"context"
	"crypto/ed25519"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"testing"
)

type unusedQuiescenceIssuer struct{}

func (*unusedQuiescenceIssuer) Certificate(context.Context) ([]byte, error) {
	panic("constructor called quiescence Certificate")
}
func (*unusedQuiescenceIssuer) SignQuiesceUsers(context.Context, string, p.TaskUserQuiescenceTicketClaims) ([]byte, error) {
	panic("constructor called quiescence signer")
}
func TestTaskQuiesceMetadataAuthority(t *testing.T) {
	o := validOptions(t)
	v, err := taskQuiescenceAuthority(o)
	require.NoError(t, err)
	require.Nil(t, v)
	for _, kind := range []string{"valid", "query-only", "typednil", "trust", "clock", "typedclock", "roots"} {
		t.Run(kind, func(t *testing.T) {
			o := validOptions(t)
			o.TaskQuiescenceIssuer = &unusedQuiescenceIssuer{}
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
			o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("constructor called clock") })
			switch kind {
			case "query-only":
				o.TaskQuiescenceIssuer = nil
			case "typednil":
				o.TaskQuiescenceIssuer = (*unusedQuiescenceIssuer)(nil)
			case "trust":
				o.PublicationTrust = nil
			case "clock":
				o.Clock = nil
			case "typedclock":
				o.Clock = testAuthorityClock(nil)
			case "roots":
				o.PublicationTrust.Roots = nil
			}
			v, err := taskQuiescenceAuthority(o)
			if kind == "valid" || kind == "query-only" {
				require.NoError(t, err)
				require.NotNil(t, v)
				require.NoError(t, validateOptions(o))
			} else {
				require.ErrorIs(t, err, ErrInvalidConfiguration)
				require.Nil(t, v)
				require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
			}
		})
	}
}

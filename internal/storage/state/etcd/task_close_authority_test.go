package etcd

import (
	"context"
	"crypto/ed25519"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"testing"
)

type unusedTaskIssuer struct{}

func (*unusedTaskIssuer) Certificate(context.Context) ([]byte, error) {
	panic("constructor called task Certificate")
}
func (*unusedTaskIssuer) SignCloseData(context.Context, string, controlprotocol.TaskCloseDataTicketClaims) ([]byte, error) {
	panic("constructor called SignCloseData")
}
func TestTaskCloseAuthority(t *testing.T) {
	o := validOptions(t)
	v, e := taskAuthority(o)
	require.NoError(t, e)
	require.Nil(t, v)
	for _, fault := range []string{"valid", "typed-nil", "trust", "clock", "typed-clock", "authority", "roots"} {
		t.Run(fault, func(t *testing.T) {
			o := validOptions(t)
			o.TaskIssuer = &unusedTaskIssuer{}
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
			o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("constructor observed clock") })
			switch fault {
			case "typed-nil":
				o.TaskIssuer = (*unusedTaskIssuer)(nil)
			case "trust":
				o.PublicationTrust = nil
			case "clock":
				o.Clock = nil
			case "typed-clock":
				o.Clock = testAuthorityClock(nil)
			case "authority":
				o.PublicationTrust.AuthorityID = "other"
			case "roots":
				o.PublicationTrust.Roots = nil
			}
			v, e := taskAuthority(o)
			if fault == "valid" {
				require.NoError(t, e)
				require.NotNil(t, v)
				require.NoError(t, validateOptions(o))
			} else {
				require.ErrorIs(t, e, ErrInvalidConfiguration)
				require.Nil(t, v)
				require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
				b, e := New(context.Background(), o)
				require.Nil(t, b)
				require.ErrorIs(t, e, ErrInvalidConfiguration)
			}
		})
	}
}

package controlrunner

import (
	"context"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
)

type healthUnlockedClock struct {
	s      *Supervisor
	base   p.AuthorityClock
	locked bool
}

func (c *healthUnlockedClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	if c.s.mu.TryLock() {
		c.s.mu.Unlock()
	} else {
		c.locked = true
	}
	return c.base.Observe(ctx)
}

func TestSupervisorHealthObservation(tst *testing.T) {
	for _, mode := range []string{"closed-gate", "admission-closed", "pending", "failed", "closed", "close-started", "no-activation", "wrong-uid", "wrong-gate", "busy", "cancel"} {
		tst.Run(mode, func(tst *testing.T) {
			s, _ := taskCloseTerminalHistory(tst)
			s.birth = s.activation.Birth()
			s.admission = true
			clock := &healthUnlockedClock{s: s, base: s.options.Clock}
			s.options.Clock = clock
			e := t.RuntimeHealthEnvelope{Version: 1, Purpose: t.RuntimeHealthPurpose, Identity: s.activation.Identity(), DataGateEpoch: s.activation.DataGateEpoch(), Nonce: strings.Repeat("a", 64)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "admission-closed":
				s.admission = false
			case "pending":
				s.failurePending.Store(true)
			case "failed":
				s.failed = true
			case "closed":
				s.closed = true
			case "close-started":
				s.closeStarted.Store(true)
			case "no-activation":
				s.activation = nil
			case "wrong-uid":
				e.Identity.Runtime.UID = "wrong"
			case "wrong-gate":
				e.DataGateEpoch++
			case "busy":
				s.mu.Lock()
				defer s.mu.Unlock()
			case "cancel":
				cancel()
			}
			response, err := s.queryRuntimeHealth(ctx, e)
			if mode == "closed-gate" || mode == "admission-closed" {
				require.NoError(tst, err)
				require.Equal(tst, "closed", response.GateState)
				require.Equal(tst, e.Identity, response.Identity)
				require.Equal(tst, e.Nonce, response.Nonce)
			} else {
				require.Error(tst, err)
				require.Equal(tst, t.RuntimeHealthResponse{}, response)
			}
			require.False(tst, clock.locked, "controlled clock I/O cannot hold Supervisor admission mutex")
		})
	}
}

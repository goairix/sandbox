package etcd

import (
	"context"
	"testing"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
)

func TestDeliveryRenewDistinguishesVerifiedTerminalFromLoss(tst *testing.T) {
	// Original protocol reconciliation fixture only, never execution evidence.
	h, sign := receiptAttributionFixture(tst)
	_, err := h.observeReceipt(context.Background(), t.EventReceipt, sign("local_terminal", h.deadline))
	require.NoError(tst, err)
	err = h.origin.RenewExecDelivery(context.Background(), h)
	require.ErrorIs(tst, err, ErrExecDeliveryTerminal)
	require.ErrorIs(tst, err, ErrConflict, "existing conflict compatibility is preserved")
	unknown, signUnknown := receiptAttributionFixture(tst)
	_, err = unknown.observeReceipt(context.Background(), t.EventReceipt, signUnknown("unknown", unknown.deadline))
	require.NoError(tst, err)
	err = unknown.origin.RenewExecDelivery(context.Background(), unknown)
	require.ErrorIs(tst, err, ErrConflict)
	require.NotErrorIs(tst, err, ErrExecDeliveryTerminal, "unknown is never classified as harmless terminal")
	// Even a later terminal cannot erase an already observed unknown condition.
	_, err = unknown.observeReceipt(context.Background(), t.EventReceipt, signUnknown("local_terminal", unknown.deadline))
	require.NoError(tst, err)
	err = unknown.origin.RenewExecDelivery(context.Background(), unknown)
	require.NotErrorIs(tst, err, ErrExecDeliveryTerminal)
}

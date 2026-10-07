package etcd

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
)

func TestDeliveryRejectsAbsentOriginalAuthorityBeforeDial(t *testing.T) {
	b := &Backend{}
	other := &Backend{}
	for _, p := range []*PreparedExecEffect{nil, {}, {origin: other}, {origin: b, draft: &execEffectDraft{}}} {
		if h, err := b.DeliverExecEffect(context.Background(), p, &controltransport.Destination{}); err == nil || h != nil {
			t.Fatal("invalid original prepared delivered")
		}
	}
	for _, h := range []*ExecDeliveryHandle{nil, {}} {
		if err := b.RenewExecDelivery(context.Background(), h); err == nil {
			t.Fatal("invalid handle renewed")
		}
		if _, err := h.Wait(context.Background(), nil, nil); err == nil {
			t.Fatal("invalid handle waited")
		}
	}
}
func TestCopiedPreparedRefusesOriginalDelivery(t *testing.T) {
	b := &Backend{}
	c := &OperationCapability{origin: b, parentCtx: context.Background()}
	d := &execEffectDraft{}
	c.execDraft = d
	p := &PreparedExecEffect{origin: b, capability: c, draft: d}
	p.self = p
	copied := *p
	if h, err := b.DeliverExecEffect(context.Background(), &copied, nil); err == nil || h != nil {
		t.Fatal("copied prepared delivered")
	}
}

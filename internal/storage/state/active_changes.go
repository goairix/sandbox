package state

import "context"

type ActiveChange struct {
	SandboxID                       string
	RecordChanged, OperationChanged bool
}

// ActiveChangeBatch is a complete revision. Reset requires authoritative
// rereads; notifications never grant admission or prove drain.
type ActiveChangeBatch struct {
	Revision int64
	Reset    bool
	Changes  []ActiveChange
}
type ActiveOperationObservation struct{ Count, Revision int64 }
type ActiveChangeRepository interface {
	WatchActiveChanges(context.Context, int64) (<-chan ActiveChangeBatch, error)
	ObserveOperations(context.Context, string) (ActiveOperationObservation, error)
}

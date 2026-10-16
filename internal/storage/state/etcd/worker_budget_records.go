package etcd

import (
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// WorkerBudgetKind selects one fixed cell-wide class of scheduling shares.
type WorkerBudgetKind uint8

const (
	WorkerBudgetRelist WorkerBudgetKind = iota + 1
	WorkerBudgetRange
	WorkerBudgetClaim
)

// WorkerBudgetReference is an owned diagnostic value, never an adoptable
// permission or a substitute for the consumer's same-transaction fences.
type WorkerBudgetReference struct {
	Session        WorkerSessionReference
	Kind           WorkerBudgetKind
	Slot           uint16
	CreateRevision int64
}

type workerBudgetLimits struct {
	name           string
	count          uint16
	offset         int
	rate, capacity uint64
}

func workerBudgetSettings(kind WorkerBudgetKind) (workerBudgetLimits, bool) {
	switch kind {
	case WorkerBudgetRelist:
		return workerBudgetLimits{name: "relist", count: 4}, true
	case WorkerBudgetRange:
		return workerBudgetLimits{name: "range", count: 20, offset: 4, rate: 1048576, capacity: 262144}, true
	case WorkerBudgetClaim:
		return workerBudgetLimits{name: "claim", count: 100, offset: 24, rate: 1, capacity: 1}, true
	}
	return workerBudgetLimits{}, false
}

func workerBudgetNamed(name string) (WorkerBudgetKind, bool) {
	switch name {
	case "relist":
		return WorkerBudgetRelist, true
	case "range":
		return WorkerBudgetRange, true
	case "claim":
		return WorkerBudgetClaim, true
	}
	return 0, false
}

type workerBudgetRecord struct {
	workerSessionRecord
	Kind string `json:"kind"`
	Slot uint16 `json:"slot"`
}

func (r workerBudgetRecord) Validate() error {
	kind, valid := workerBudgetNamed(r.Kind)
	limits, ok := workerBudgetSettings(kind)
	if r.workerSessionRecord.Validate() != nil || !valid || !ok || r.Slot >= limits.count {
		return ErrInvalidRecord
	}
	return nil
}

func (n Namespace) workerBudgetKey(kind WorkerBudgetKind, slot uint16) (string, error) {
	limits, ok := workerBudgetSettings(kind)
	if !ok || slot >= limits.count {
		return "", ErrInvalidRecord
	}
	return n.Key("workers", "budget", limits.name, fmt.Sprintf("%02x", slot))
}

func decodeWorkerBudgetRecord(n Namespace, kv *mvccpb.KeyValue, dst *workerBudgetRecord) error {
	if dst == nil {
		return ErrCorruptRecord
	}
	var fresh workerBudgetRecord
	if workerDecodeLeased(kv, &fresh) != nil || fresh.Namespace != n.Root() {
		return ErrCorruptRecord
	}
	kind, ok := workerBudgetNamed(fresh.Kind)
	if !ok {
		return ErrCorruptRecord
	}
	key, err := n.workerBudgetKey(kind, fresh.Slot)
	if err != nil || string(kv.Key) != key || fresh.LeaseID != kv.Lease {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}

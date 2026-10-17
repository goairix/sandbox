package etcd

import (
	"bytes"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// coldQuotaWorkerBaseComparisons shares only the journal/Stage composition
// invariant; each caller still owns its exact body shape. The original body
// and W24 both start with identity Value/Lease and restore Value/Lease. Both
// complete protobuf prefixes must match this backend's fresh base4 before its
// duplicate is removed. Key-based deduplication would lose required targets,
// while comparing only the two inputs could accept identically corrupted bases.
// All twenty non-base worker comparisons survive unchanged and in order.
func (b *Backend) coldQuotaWorkerBaseComparisons(body, worker []clientv3.Cmp) ([]clientv3.Cmp, error) {
	if b == nil || len(body) < 4 || len(worker) != 24 {
		return nil, ErrInvalidMutation
	}
	base := taskProtoComparisons(b.baseComparisons())
	for i := range base {
		expected, err := base[i].Marshal()
		if err != nil {
			return nil, ErrInvalidMutation
		}
		for _, candidate := range []clientv3.Cmp{body[i], worker[i]} {
			comparison := pb.Compare(candidate)
			wire, err := comparison.Marshal()
			if err != nil || !bytes.Equal(wire, expected) {
				return nil, ErrInvalidMutation
			}
		}
	}
	comparisons := make([]clientv3.Cmp, 0, len(body)+len(worker)-len(base))
	comparisons = append(comparisons, body...)
	comparisons = append(comparisons, worker[len(base):]...)
	return comparisons, nil
}

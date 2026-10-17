package nodeobserver

import (
	"fmt"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Reply allowance does not enlarge the native request allowance. This shape is
// below 256 KiB but above 128 KiB and must be rejected before any native RPC.
func TestNodeObserverAuthorityRequestUses128KiBBound(t *testing.T) {
	s := &authoritySnapshot{keys: p.TaskRuntimeTerminationKeys{Identity: "/a/b/c/meta/identity", Restore: "/a/b/c/meta/restore_epoch", OperationsPrefix: "/a/b/c/p/00/sandboxes/s/operations/"}}
	point := func(key string) *mvccpb.KeyValue {
		return &mvccpb.KeyValue{Key: []byte(key), Value: []byte(`{"version":1}`), CreateRevision: 1, ModRevision: 1, Version: 1}
	}
	s.identity, s.restore = point(s.keys.Identity), point(s.keys.Restore)
	s.complete, s.intent = point("/a/b/c/complete"), point("/a/b/c/intent")
	s.claim, s.guard = point("/a/b/c/claim"), point("/a/b/c/guard")
	for i := range s.domain {
		s.domain[i] = point(fmt.Sprintf("/a/b/c/domain/%d", i))
	}
	for i := range s.history {
		s.history[i] = point(fmt.Sprintf("/a/b/c/history/%d", i))
	}
	s.domain[0].Value = []byte(strings.Repeat("x", 140*1024))
	request := &pb.TxnRequest{Success: []*pb.RequestOp{protoPoint(s.keys.Identity)}}
	for _, comparison := range s.comparisons() {
		wire := pb.Compare(comparison)
		request.Compare = append(request.Compare, &wire)
	}
	if request.Size() <= 128*1024 || request.Size() >= 256*1024 {
		t.Fatalf("test must exercise request/reply split, got %d bytes", request.Size())
	}
	if _, err := s.guardRequest(); err == nil {
		t.Fatalf("accepted native request of %d bytes above 128 KiB", request.Size())
	}
}

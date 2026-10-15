package nodeobserver

import (
	"context"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func TestNodeObserverAuthorityFreshClockOnlyShrinks(t *testing.T) {
	now := time.Now().UTC()
	c := p.TaskRuntimeTerminationTicketClaims{NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Second)}
	s := &authoritySnapshot{cutoff: time.Now().Add(20 * time.Second)}
	original := s.cutoff
	if err := s.restrictTicketClock(context.Background(), p.ClockObservation{UTC: now, Uncertainty: time.Millisecond}, c); err != nil {
		t.Fatal(err)
	}
	if s.cutoff != original {
		t.Fatal("fresh observation extended earlier native/Lease cutoff")
	}
	if err := s.restrictTicketClock(context.Background(), p.ClockObservation{UTC: c.NotAfter.Add(-2 * time.Second), Uncertainty: time.Second}, c); err != nil {
		t.Fatal(err)
	}
	shortened := s.cutoff
	if !shortened.Before(original) {
		t.Fatal("forward clock/increased uncertainty did not shorten cutoff")
	}
	if err := s.restrictTicketClock(context.Background(), p.ClockObservation{UTC: now.Add(-time.Second)}, c); err != nil {
		t.Fatal(err)
	}
	if s.cutoff != shortened {
		t.Fatal("backward clock/reduced uncertainty extended cutoff")
	}
	if err := s.restrictTicketClock(context.Background(), p.ClockObservation{UTC: c.NotAfter.Add(-time.Millisecond), Uncertainty: time.Second}, c); err == nil {
		t.Fatal("fresh uncertainty overlapping original expiry accepted")
	}
}

func TestNodeObserverAuthorityFixedGuard(t *testing.T) {
	s := &authoritySnapshot{keys: p.TaskRuntimeTerminationKeys{Identity: "/a/b/c/meta/identity", Restore: "/a/b/c/meta/restore_epoch", OperationsPrefix: "/a/b/c/p/00/sandboxes/s/operations/"}}
	makeKV := func(key string) *mvccpb.KeyValue {
		return &mvccpb.KeyValue{Key: []byte(key), Value: []byte(`{"version":1}`), CreateRevision: 1, ModRevision: 2, Version: 2}
	}
	s.identity, s.restore = makeKV(s.keys.Identity), makeKV(s.keys.Restore)
	s.complete, s.intent = makeKV("/a/b/c/complete"), makeKV("/a/b/c/intent")
	s.claim, s.guard = makeKV("/a/b/c/claim"), makeKV("/a/b/c/guard")
	s.claim.Lease, s.guard.Lease = 3, 3
	for i := range s.domain {
		s.domain[i] = makeKV("/a/b/c/domain/" + strings.Repeat("x", i+1))
	}
	for i := range s.history {
		s.history[i] = makeKV("/a/b/c/history/" + strings.Repeat("x", i+1))
	}
	request, err := s.guardRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Compare) != 59 || len(request.Success) != 1 || len(request.Failure) != 0 || len(request.Compare)+len(request.Success) != 60 {
		t.Fatal("guard does not have literal 59+1 native shape")
	}
	if request.Success[0].GetRequestRange() == nil || string(request.Success[0].GetRequestRange().Key) != s.keys.Identity {
		t.Fatal("guard success is not the fixed identity point read")
	}
	last := request.Compare[58]
	if last.Target != pb.Compare_VERSION || last.GetVersion() != 0 || string(last.Key) != s.keys.OperationsPrefix || len(last.RangeEnd) == 0 {
		t.Fatal("guard omitted whole operations-prefix Version0")
	}
	s.domain[0].Value = []byte(strings.Repeat("x", 256*1024))
	if _, err := s.guardRequest(); err == nil {
		t.Fatal("native guard request exceeds wire budget")
	}
}

func TestNodeObserverAuthorityRejectMalformedGuardReply(t *testing.T) {
	s := &authoritySnapshot{reader: &authorityReader{identity: []byte(`{"version":1}`), clusterID: 7}, keys: p.TaskRuntimeTerminationKeys{Identity: "/identity"}}
	good := func() *pb.TxnResponse {
		return &pb.TxnResponse{Header: &pb.ResponseHeader{ClusterId: 7, Revision: 9}, Succeeded: true, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Count: 1, Kvs: []*mvccpb.KeyValue{{Key: []byte("/identity"), Value: s.reader.identity, CreateRevision: 1, ModRevision: 1, Version: 1}}}}}}}
	}
	if err := s.validateGuardReply(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*pb.TxnResponse){
		"nested header":   func(r *pb.TxnResponse) { r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{ClusterId: 8} },
		"kv unknown":      func(r *pb.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].XXX_unrecognized = []byte{1} },
		"future revision": func(r *pb.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].ModRevision = 10 },
		"invalid birth":   func(r *pb.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].CreateRevision = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := good()
			mutate(r)
			if err := s.validateGuardReply(r); err == nil {
				t.Fatal("malformed native guard identity accepted")
			}
		})
	}
}

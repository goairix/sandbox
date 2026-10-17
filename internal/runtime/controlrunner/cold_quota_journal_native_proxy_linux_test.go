//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"encoding/json"
	"sync"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// coldQuotaJournalNativeObservation exists only in the two journal scenes.
// It observes actual upstream operations and may discard one successful CAS
// ACK. The original Backend alone owns admission, Grant and refund decisions.
type coldQuotaJournalNativeObservation struct {
	mu sync.Mutex

	journalKey, returnKey string
	dropACK, dropped      bool
	watchGrants           bool
	candidate             state.StageReference
	journalRequests       []*pb.TxnRequest
	grants                []coldQuotaJournalNativeGrant
	reads                 []*pb.RangeRequest
}

type coldQuotaJournalNativeGrant struct {
	candidate state.StageReference
	response  *pb.RangeResponse
	err       error
}

func (o *coldQuotaJournalNativeObservation) observeRead(request *pb.RangeRequest) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reads = append(o.reads, request)
}

// The upstream KV client bypasses the proxy: this bounded, linearizable read
// cannot recursively enter the observer or allocate a lease. Record its result
// before forwarding Grant, without replacing any actual server response.
func (o *coldQuotaJournalNativeObservation) observeBeforeGrant(ctx context.Context, upstream pb.KVClient) {
	o.mu.Lock()
	if !o.watchGrants {
		o.mu.Unlock()
		return
	}
	key, candidate := o.journalKey, o.candidate
	o.mu.Unlock()

	response, err := upstream.Range(ctx, &pb.RangeRequest{Key: []byte(key)})

	o.mu.Lock()
	o.grants = append(o.grants, coldQuotaJournalNativeGrant{candidate: candidate, response: response, err: err})
	o.mu.Unlock()
}

func (o *coldQuotaJournalNativeObservation) observeTransaction(request *pb.TxnRequest, response *pb.TxnResponse, err error, label string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, operations := range [][]*pb.RequestOp{request.Success, request.Failure} {
		for _, operation := range operations {
			if point := operation.GetRequestRange(); point != nil {
				o.reads = append(o.reads, point)
			}
		}
	}

	for _, operation := range request.Success {
		put := operation.GetRequestPut()
		if put == nil {
			continue
		}
		if string(put.Key) == o.returnKey {
			label = "quota-commit"
		}
		if string(put.Key) != o.journalKey {
			continue
		}
		label = "quota-journal"
		o.journalRequests = append(o.journalRequests, request)
		if err != nil || response == nil || !response.Succeeded {
			continue
		}
		var record struct {
			Reference state.StageReference `json:"reference"`
		}
		if json.Unmarshal(put.Value, &record) == nil {
			o.candidate = record.Reference
		}
		if o.dropACK && !o.dropped {
			o.dropped = true
			return label, true
		}
	}
	return label, false
}

// Decode the original reference from actual journal bytes without exposing
// private Backend state or manufacturing a continuation capability.
func coldQuotaJournalNativeRecord(kv *mvccpb.KeyValue) (state.StageReference, error) {
	var record struct {
		Reference state.StageReference `json:"reference"`
	}
	err := json.Unmarshal(kv.Value, &record)
	return record.Reference, err
}

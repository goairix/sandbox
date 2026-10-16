package etcd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func enrolledPublicationAuthority(o Options) (*controlprotocol.ManagementVerifier, error) {
	if o.EnrolledPublication == nil {
		return nil, nil
	}
	p := o.EnrolledPublication
	if o.PublicationTrust == nil || p.MaxActive < 1 || p.MaxActive > 32 || !validHexDigest(p.ContractDigest) {
		return nil, fmt.Errorf("%w: enrolled publication profile", ErrInvalidConfiguration)
	}
	v, err := controlprotocol.NewManagementVerifier(controlprotocol.TrustBinding{Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch}, o.PublicationTrust.Roots)
	if err != nil {
		return nil, fmt.Errorf("%w: enrolled publication trust: %v", ErrInvalidConfiguration, err)
	}
	return v, nil
}

// This closed domain has exactly the 24 original claim fences, 18 preparation
// fences and four publication absences. It never enters the generic Stage's
// fourteen-node reservation, which remains unchanged for all legacy callers.
func prepareEnrolledMutation(n Namespace, input Mutation) (Mutation, string, error) {
	return prepareEnrolledMutationDomain(n, input, enrolledPublicationStage, 8)
}

func prepareEnrolledDueMutation(n Namespace, input Mutation) (Mutation, string, error) {
	return prepareEnrolledMutationDomain(n, input, enrolledDuePublicationStage, 9)
}

func prepareEnrolledMutationDomain(n Namespace, input Mutation, stage string, writes int) (Mutation, string, error) {
	if len(n.Root()) > 512 || len(input.Comparisons) != 46 || len(input.Writes) != writes {
		return Mutation{}, "", ErrInvalidMutation
	}
	owned := Mutation{Comparisons: make([]clientv3.Cmp, 46), Writes: make([]Write, writes)}
	for i, c := range input.Comparisons {
		if !validNamespaceKey(n, string(c.Key)) || len(c.RangeEnd) != 0 || !validComparison(c) {
			return Mutation{}, "", ErrInvalidMutation
		}
		owned.Comparisons[i] = copyComparison(c)
	}
	seen := make(map[string]bool, writes)
	for i, w := range input.Writes {
		if w.Delete || !validNamespaceKey(n, w.Key) || reservedStageKey(n, w.Key) || seen[w.Key] || len(w.Value) > maxRecordBytes {
			return Mutation{}, "", ErrInvalidMutation
		}
		seen[w.Key] = true
		owned.Writes[i] = Write{Key: w.Key, Value: bytes.Clone(w.Value)}
	}
	wire, err := json.Marshal(owned)
	if err != nil {
		return Mutation{}, "", ErrInvalidMutation
	}
	hash := sha256.Sum256(append([]byte(stage+"\x00"), wire...))
	return owned, hex.EncodeToString(hash[:]), nil
}

func enrolledGuard(ref StageReference, lease int64) string {
	wire, _ := json.Marshal(struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}{ref, lease})
	return string(wire)
}

// Preflight each complete native request and each complete branch response
// independently; conservative maximal scalar headers never become authority.
func (b *Backend) preflightEnrolledStage(ref StageReference, mutation Mutation, lease, revision int64) error {
	writes := 8
	if ref.StageID == enrolledDuePublicationStage {
		writes = 9
	}
	if !supportedEnrolledPublicationStage(ref.StageID) || ref.Namespace != b.namespace.Root() || ref.RestoreEpoch != b.restoreEpoch || len(mutation.Comparisons) != 46 || len(mutation.Writes) != writes {
		return ErrInvalidMutation
	}
	guardKey, receiptKey, err := b.stageKeys(ref)
	if err != nil {
		return err
	}
	guard := enrolledGuard(ref, lease)
	committed, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return err
	}
	aborted, err := encodeReceipt(ref, OutcomeAborted)
	if err != nil {
		return err
	}
	beginCmp := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	begin := &pb.TxnRequest{Compare: taskProtoComparisons(beginCmp), Success: []*pb.RequestOp{taskProtoPut(guardKey, guard, lease)}, Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey})}
	cmps := append(b.baseComparisons(), mutation.Comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Value(guardKey), "=", guard), clientv3.Compare(clientv3.LeaseValue(guardKey), "=", lease), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", revision), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	commit := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: append(taskProtoWrites(mutation.Writes), taskProtoPut(receiptKey, committed, 0)), Failure: taskProtoPoints([]string{receiptKey})}
	if ref.StageID == enrolledDuePublicationStage {
		commit.Failure = nil
	}
	keys := []string{b.identityKey, b.restoreKey, receiptKey, guardKey}
	reads := taskProtoPoints(keys)
	arbitrate := &pb.TxnRequest{Compare: taskProtoComparisons(b.baseComparisons()), Success: reads, Failure: reads}
	if len(commit.Compare) != 54 || len(commit.Success) != writes+1 || len(commit.Compare)+len(commit.Success)+len(commit.Failure) != 64 || len(arbitrate.Compare)+len(arbitrate.Success)+len(arbitrate.Failure) != 12 {
		return ErrInvalidMutation
	}
	replies := []*pb.TxnResponse{taskProtoPutResponse(1), taskProtoPutResponse(writes + 1), taskProtoResponse(keys[:2], []string{b.identityValue, b.restoreEpoch}), taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch, committed, guard})}
	if ref.StageID == enrolledPublicationStage {
		replies = append(replies, taskProtoResponse([]string{receiptKey}, []string{committed}), taskProtoResponse([]string{receiptKey}, []string{aborted}))
	} else {
		empty := taskProtoPutResponse(0)
		empty.Succeeded = false
		replies = append(replies, empty)
	}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{begin, commit, arbitrate}, replies)
}

func (b *Backend) preflightEnrolledMaximum(ref StageReference, mutation Mutation) error {
	return b.preflightEnrolledStage(ref, mutation, math.MaxInt64, math.MaxInt64)
}

package etcd

import (
	"fmt"
	"math"
	"strings"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Renewal uses three requests under the old deadline. Reserve those requests
// plus one second after the finite metadata phase; callers require remaining
// original lease time strictly greater than margin, without changing its clock.
func cleanupBirthPhaseTiming(request time.Duration, terminal bool) (phase, margin time.Duration, err error) {
	if request <= 0 || request > 3*time.Second {
		return 0, 0, ErrInvalidConfiguration
	}
	requests := time.Duration(4)
	if terminal {
		requests = 5
	}
	phase = requests*request + time.Second
	return phase, phase + 3*request + time.Second, nil
}

// This exact routing/cap set is shared by the concrete native cohort and its
// accounting projection. The receipt digest placeholder locates only a key;
// actual receipt bytes supply and authenticate that digest in the birth join.
func (b *Backend) cleanupBirthCohortKeys(ref TaskReference, initial StageAttemptLocator) ([]string, []int, error) {
	if b.validateTaskReference(ref) != nil || !taskAttemptMatches(initial, ref) {
		return nil, nil, ErrInvalidRecord
	}
	task, err := b.namespace.taskKey(ref)
	if err != nil {
		return nil, nil, err
	}
	intent, err := b.namespace.cleanupIntentKey(ref)
	if err != nil {
		return nil, nil, err
	}
	link, err := b.namespace.taskLinkKey(ref)
	if err != nil {
		return nil, nil, err
	}
	_, receipt, err := b.stageKeys(initial.reference(cleanupWirePaddingDigest()))
	if err != nil {
		return nil, nil, err
	}
	execution, err := b.namespace.cleanupExecutionKey(ref)
	if err != nil {
		return nil, nil, err
	}
	pending, err := b.namespace.cleanupPendingKey(ref)
	if err != nil {
		return nil, nil, err
	}
	_, _, release, err := b.namespace.localRuntimeReleaseKeys(ref)
	if err != nil {
		return nil, nil, err
	}
	return []string{task, intent, link, receipt, execution, pending, release}, []int{4096, 4096, 2048, 2048, 4096, 2048, 8192}, nil
}

// Replies use maximal 1024-byte keys independently of today's routing lengths.
// Every KV/header scalar is maximal through taskProtoResponse. These buffers
// never enter native requests, proof loaders or retained observations.
func cleanupBirthPointReply(caps []int) *pb.TxnResponse {
	keys, values := make([]string, len(caps)), make([]string, len(caps))
	for i, limit := range caps {
		keys[i] = strings.Repeat("k", maxKeyBytes)
		values[i] = cleanupWirePadding(limit)
	}
	return taskProtoResponse(keys, values)
}

func cleanupBirthPointsProjection(name string, cmps []clientv3.Cmp, keys []string, caps []int) cleanupWireProjection {
	points := taskProtoPoints(keys)
	return cleanupWireProjection{
		name: name, request: &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: points, Failure: points},
		success: cleanupBirthPointReply(caps), failure: cleanupBirthPointReply(caps),
	}
}

// Pad a copy only after validating the original request. Otherwise a malformed
// oversized actual comparison could be hidden by replacing it with a smaller
// accounting value. Permanent zero leases stay zero; positive pins use MaxInt64.
func (b *Backend) maximumCleanupBirthComparisons(cmps []*pb.Compare) []*pb.Compare {
	maximum := make([]clientv3.Cmp, len(cmps))
	for i, cmp := range cmps {
		maximum[i] = copyComparison(clientv3.Cmp(*cmp))
		switch value := maximum[i].TargetUnion.(type) {
		case *pb.Compare_Value:
			limit := workerRecordBytes
			if string(cmp.Key) == b.identityKey {
				limit = maxIdentityJSONBytes
			} else if string(cmp.Key) == b.restoreKey {
				limit = maxNamespaceComponentBytes
			}
			value.Value = []byte(cleanupWirePadding(limit))
		case *pb.Compare_CreateRevision:
			if value.CreateRevision > 0 {
				value.CreateRevision = math.MaxInt64
			}
		case *pb.Compare_ModRevision:
			if value.ModRevision > 0 {
				value.ModRevision = math.MaxInt64
			}
		case *pb.Compare_Lease:
			if value.Lease > 0 {
				value.Lease = math.MaxInt64
			}
		}
	}
	return taskProtoComparisons(maximum)
}

// terminal selects only a dimension profile for future proof accounting. It
// grants no terminal authority and never calls a proof loader. Birth routing is
// shared with its cohort; terminal history keys depend on not-yet-read signed
// data, so their maximal placeholders deliberately make no routing claim.
func (b *Backend) cleanupBirthWireProjections(cmps []clientv3.Cmp, ref TaskReference, initial StageAttemptLocator, session *WorkerSession, terminal bool) ([]cleanupWireProjection, error) {
	if b == nil || b.workerControl == nil || session == nil || len(cmps) != 20 {
		return nil, ErrInvalidRecord
	}
	keys, caps, err := b.cleanupBirthCohortKeys(ref, initial)
	if err != nil {
		return nil, err
	}
	for _, cmp := range cmps {
		if !validComparison(cmp) || !validNamespaceKey(b.namespace, string(cmp.Key)) {
			return nil, ErrInvalidMutation
		}
	}
	guard := cleanupWireProjection{
		name:    "birth-worker-guard",
		request: &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints([]string{b.identityKey}), Failure: taskProtoPoints(b.workerFailureKeys(session))},
		success: cleanupBirthPointReply([]int{4096}), failure: cleanupBirthPointReply([]int{4096, 128, 2048, 2048}),
	}
	profiles := []cleanupWireProjection{guard}
	appendBusiness := func(name string, businessKeys []string, businessCaps []int) {
		all := append([]string{b.identityKey, b.restoreKey}, businessKeys...)
		allCaps := append([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes}, businessCaps...)
		profiles = append(profiles, cleanupBirthPointsProjection(name, b.baseComparisons(), all, allCaps))
	}
	if !terminal {
		appendBusiness("birth-intent", []string{keys[1]}, []int{4096})
		appendBusiness("birth-cohort", keys, caps)
	} else {
		termination, provider, release, err := b.namespace.localRuntimeReleaseKeys(ref)
		if err != nil {
			return nil, err
		}
		appendBusiness("release-observation", []string{release}, []int{8192})
		appendBusiness("release-terminal-provider", []string{termination, provider}, []int{16384, 16384})
		var historyKeys []string
		for i := 0; i < 9; i++ {
			historyKeys = append(historyKeys, b.namespace.Root()+strings.Repeat("k", maxKeyBytes-len(b.namespace.Root())-2)+fmt.Sprintf("%02d", i))
		}
		appendBusiness("release-signed-history", historyKeys, []int{4096, 8192, 32768, 4096, 8192, 8192, 8192, maxRecordBytes, 8192})
	}
	profiles = append(profiles, guard)

	// Exact conservative sums include a successful prefix and the worst final
	// guard failure. They are byte bounds, not performance or memory estimates.
	bound, nodes := 87552, []int{25, 10, 22, 25}
	if terminal {
		bound, nodes = 260480, []int{25, 10, 12, 26, 25}
	}
	used := 0
	for i := range profiles {
		profile := &profiles[i]
		if err := validateCleanupWire(*profile); err != nil {
			return nil, err
		}
		if len(profile.request.Compare)+len(profile.request.Success)+len(profile.request.Failure) != nodes[i] {
			return nil, ErrInvalidMutation
		}
		// Both guard profiles initially share a request; copy it before padding.
		request := *profile.request
		request.Compare = b.maximumCleanupBirthComparisons(request.Compare)
		profile.request = &request
		if err := validateCleanupWire(*profile); err != nil {
			return nil, err
		}
		if used+profile.failure.Size() > bound {
			return nil, ErrInvalidMutation
		}
		used += profile.success.Size()
	}
	if used > bound || bound > maxMutationBytes {
		return nil, ErrInvalidMutation
	}
	return profiles, nil
}

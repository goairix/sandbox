package etcd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	maxStageOperations = 64
	maxRecordBytes     = 64 * 1024
	maxMutationBytes   = 256 * 1024
	maxKeyBytes        = 1024
	// Reserve the base, guard and receipt comparisons plus response reads.
	stageProtocolOperations = 14
)

// Write changes one permanent business key. Delete and Value are mutually
// exclusive; stage writes never attach a Lease or delete a key range.
type Write struct {
	Key    string
	Value  []byte
	Delete bool
}

// Mutation describes one stage's immutable transaction. Comparisons may cover
// bounded ranges inside the namespace; writes are only individual business keys.
type Mutation struct {
	Comparisons []clientv3.Cmp
	Writes      []Write
}

func prepareMutation(n Namespace, input Mutation) (Mutation, string, error) {
	invalid := func(reason string) (Mutation, string, error) {
		return Mutation{}, "", fmt.Errorf("%w: %s", ErrInvalidMutation, reason)
	}
	if len(input.Writes) == 0 || len(input.Writes)+len(input.Comparisons)+stageProtocolOperations > maxStageOperations {
		return invalid("empty mutation or transaction operation budget exceeded")
	}
	result := Mutation{Comparisons: make([]clientv3.Cmp, len(input.Comparisons)), Writes: make([]Write, len(input.Writes))}
	for i, comparison := range input.Comparisons {
		if !validNamespaceKey(n, string(comparison.Key)) {
			return invalid("comparison key is outside the namespace")
		}
		if len(comparison.RangeEnd) > maxKeyBytes || len(comparison.RangeEnd) > 0 && (bytes.Compare(comparison.RangeEnd, comparison.Key) <= 0 || bytes.Compare(comparison.RangeEnd, []byte(clientv3.GetPrefixRangeEnd(n.Root()))) > 0) {
			return invalid("comparison range is outside the namespace")
		}
		if !validComparison(comparison) {
			return invalid("malformed comparison")
		}
		// Cmp contains slices and a protobuf oneof; a shallow copy would allow
		// caller mutation after the guard's digest was fixed.
		result.Comparisons[i] = copyComparison(comparison)
	}
	seen := make(map[string]struct{}, len(input.Writes))
	for i, write := range input.Writes {
		if !validNamespaceKey(n, write.Key) || reservedStageKey(n, write.Key) {
			return invalid("write key is outside business state")
		}
		if _, exists := seen[write.Key]; exists {
			return invalid("duplicate write key")
		}
		seen[write.Key] = struct{}{}
		if len(write.Value) > maxRecordBytes || (write.Delete && len(write.Value) != 0) {
			return invalid("invalid delete or record size exceeded")
		}
		result.Writes[i] = Write{Key: write.Key, Value: bytes.Clone(write.Value), Delete: write.Delete}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return invalid("cannot encode mutation")
	}
	// JSON accounting includes base64 expansion and is deliberately more
	// conservative than protobuf payload size, with space for protocol keys.
	if len(encoded)+16*maxKeyBytes > maxMutationBytes {
		return invalid("transaction byte budget exceeded")
	}
	digest := sha256.Sum256(encoded)
	return result, hex.EncodeToString(digest[:]), nil
}

func validNamespaceKey(n Namespace, key string) bool {
	if len(key) > maxKeyBytes || !strings.HasPrefix(key, n.Root()) {
		return false
	}
	relative := strings.TrimPrefix(key, n.Root())
	for _, segment := range strings.Split(relative, "/") {
		if !validSegment(segment) {
			return false
		}
	}
	return true
}

func reservedStageKey(n Namespace, key string) bool {
	parts := strings.Split(strings.TrimPrefix(key, n.Root()), "/")
	return parts[0] == "meta" || parts[0] == "command-issuers" || (parts[0] == "p" && len(parts) >= 3 && (parts[2] == "attempts" || parts[2] == "stages"))
}

func validComparison(c clientv3.Cmp) bool {
	if c.Result < pb.Compare_EQUAL || c.Result > pb.Compare_NOT_EQUAL || len(c.XXX_unrecognized) != 0 {
		return false
	}
	switch value := c.TargetUnion.(type) {
	case *pb.Compare_Value:
		return value != nil && c.Target == pb.Compare_VALUE && len(value.Value) <= maxRecordBytes
	case *pb.Compare_Version:
		return value != nil && c.Target == pb.Compare_VERSION && value.Version >= 0
	case *pb.Compare_CreateRevision:
		return value != nil && c.Target == pb.Compare_CREATE && value.CreateRevision >= 0
	case *pb.Compare_ModRevision:
		return value != nil && c.Target == pb.Compare_MOD && value.ModRevision >= 0
	case *pb.Compare_Lease:
		return value != nil && c.Target == pb.Compare_LEASE && value.Lease >= 0
	default:
		return false
	}
}

func copyComparison(c clientv3.Cmp) clientv3.Cmp {
	copy := clientv3.Cmp{Key: bytes.Clone(c.Key), RangeEnd: bytes.Clone(c.RangeEnd), Target: c.Target, Result: c.Result}
	switch value := c.TargetUnion.(type) {
	case *pb.Compare_Value:
		copy.TargetUnion = &pb.Compare_Value{Value: bytes.Clone(value.Value)}
	case *pb.Compare_Version:
		copy.TargetUnion = &pb.Compare_Version{Version: value.Version}
	case *pb.Compare_CreateRevision:
		copy.TargetUnion = &pb.Compare_CreateRevision{CreateRevision: value.CreateRevision}
	case *pb.Compare_ModRevision:
		copy.TargetUnion = &pb.Compare_ModRevision{ModRevision: value.ModRevision}
	case *pb.Compare_Lease:
		copy.TargetUnion = &pb.Compare_Lease{Lease: value.Lease}
	}
	return copy
}

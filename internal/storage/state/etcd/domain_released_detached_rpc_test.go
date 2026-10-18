package etcd

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These are independent transport allowances, not historical record schemas.
// Opaque model bytes never enter an authority/proof decoder or effect API.
func releasedDetachedTransportCaps() []int {
	return []int{4096, 32768, 4096, 8192, 8192, 8192, 32768, 65536, 4096, 8192, 16384, 8192, 8192}
}

func TestReleasedDetachedTransportLegacyDimensions(t *testing.T) {
	profiles := []struct {
		profile releasedReadProfile
		caps    []int
	}{
		{releasedPlacement, []int{4096}},
		{releasedCurrent, []int{4096, 1024, 4096, 2048}},
		{releasedIntent, []int{4096}},
		{releasedCohort, []int{4096, 4096, 2048, 2048}},
		{releasedSeal, []int{8192}},
		{releasedTermination, []int{16384, 16384}},
		{releasedHistory, []int{4096, 8192, 32768, 4096, 8192, 8192, 8192, 65536, 8192}},
		{releasedFinal, []int{4096, 1024, 4096, 4096, 4096}},
		{releasedSuccessorFinal, []int{4096, 1024, 4096, 4096, 4096, 4096, 4096}},
	}
	for ordinal, profile := range profiles {
		require.Equal(t, releasedReadProfile(ordinal), profile.profile, "preserve the original private caller's enum values")
		require.Equal(t, profile.caps, releasedReadCaps(profile.profile))
		require.Equal(t, 18, releasedReadPinLimit(profile.profile))
	}
	require.NoError(t, checkReleasedReadBudget())
	require.Nil(t, releasedReadCaps(releasedReadProfile(255)))
	require.Zero(t, releasedReadPinLimit(releasedReadProfile(255)))
}

func TestReleasedDetachedTransportDimensions(t *testing.T) {
	for _, profile := range []struct {
		name    string
		profile releasedReadProfile
		caps    []int
		pins    int
	}{
		{"history", releasedDetachedHistory, releasedDetachedTransportCaps(), 18},
		{"final", releasedDetachedFinal, []int{4096, 1024, 4096, 4096, 4096}, 22},
		{"successor-final", releasedDetachedSuccessorFinal, []int{4096, 1024, 4096, 4096, 4096, 4096, 4096}, 22},
	} {
		t.Run(profile.name, func(t *testing.T) {
			require.Equal(t, profile.caps, releasedReadCaps(profile.profile))
			require.Equal(t, profile.pins, releasedReadPinLimit(profile.profile))
		})
	}
}

// Fill actual host server points with opaque bytes at every transport cap.
// They establish flat-envelope transport behavior only, not release history.
func releasedDetachedTransportSeed(t *testing.T, b *Backend, server *workerTestServer, caps []int, pinCount int) ([]string, []clientv3.Cmp) {
	t.Helper()
	keys := make([]string, len(caps))
	for i, cap := range caps {
		keys[i] = fmt.Sprintf("%stransport-point-%02d", b.namespace.Root(), i)
		server.put(keys[i], strings.Repeat("v", cap), 0)
	}
	pins := make([]clientv3.Cmp, pinCount)
	for i := range pins {
		key := fmt.Sprintf("%stransport-pin-%02d", b.namespace.Root(), i)
		server.put(key, "opaque-pin", 0)
		pins[i] = clientv3.Compare(clientv3.ModRevision(key), "=", server.point(key).ModRevision)
	}
	return keys, pins
}

func releasedDetachedTransportRequests(server *workerTestServer) []*pb.TxnRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), server.requests...)
}

func TestReleasedDetachedTransportPointAndPinBoundaries(t *testing.T) {
	for _, profile := range []struct {
		name    string
		profile releasedReadProfile
		caps    []int
		limit   int
	}{
		{"legacy-final", releasedFinal, []int{4096, 1024, 4096, 4096, 4096}, 18},
		{"detached-history", releasedDetachedHistory, releasedDetachedTransportCaps(), 18},
		{"detached-final", releasedDetachedFinal, []int{4096, 1024, 4096, 4096, 4096}, 22},
		{"detached-successor-final", releasedDetachedSuccessorFinal, []int{4096, 1024, 4096, 4096, 4096, 4096, 4096}, 22},
	} {
		t.Run(profile.name, func(t *testing.T) {
			b, server := workerTestBackend(t)
			keys, pins := releasedDetachedTransportSeed(t, b, server, profile.caps, profile.limit)
			before := len(releasedDetachedTransportRequests(server))
			points, revision, err := b.readReleasedPoints(context.Background(), profile.profile, keys, pins)
			require.NoError(t, err)
			require.Positive(t, revision)
			require.Len(t, points, len(keys))
			require.Len(t, releasedDetachedTransportRequests(server), before+1)

			extra := append(append([]clientv3.Cmp{}, pins...), pins[0])
			before = len(releasedDetachedTransportRequests(server))
			_, _, err = b.readReleasedPoints(context.Background(), profile.profile, keys, extra)
			require.Error(t, err, "profile-specific overflow must precede dispatch")
			require.Len(t, releasedDetachedTransportRequests(server), before)
		})
	}
}

func TestReleasedDetachedTransportInputRefusal(t *testing.T) {
	b, server := workerTestBackend(t)
	keys, pins := releasedDetachedTransportSeed(t, b, server, releasedDetachedTransportCaps(), 1)
	for _, fault := range []string{"unknown", "cardinality", "foreign-key", "foreign-pin", "ranged-pin", "zero-pin", "twenty-three-pins"} {
		t.Run(fault, func(t *testing.T) {
			profile := releasedDetachedHistory
			badKeys := append([]string{}, keys...)
			badPins := append([]clientv3.Cmp{}, pins...)
			switch fault {
			case "unknown":
				profile = releasedReadProfile(255)
			case "cardinality":
				badKeys = badKeys[:12]
			case "foreign-key":
				badKeys[0] = "/foreign/point"
			case "foreign-pin":
				badPins[0] = clientv3.Compare(clientv3.ModRevision("/foreign/pin"), "=", int64(1))
			case "ranged-pin":
				badPins[0].RangeEnd = []byte(string(badPins[0].Key) + "z")
			case "zero-pin":
				badPins[0] = clientv3.Compare(clientv3.ModRevision(string(pins[0].Key)), "=", int64(0))
			case "twenty-three-pins":
				for len(badPins) < 23 {
					badPins = append(badPins, pins[0])
				}
			}
			before := len(releasedDetachedTransportRequests(server))
			_, _, err := b.readReleasedPoints(context.Background(), profile, badKeys, badPins)
			require.Error(t, err)
			require.Len(t, releasedDetachedTransportRequests(server), before)
		})
	}
}

func TestReleasedDetachedTransportFlatThirteenParser(t *testing.T) {
	b, server := workerTestBackend(t)
	caps := releasedDetachedTransportCaps()
	keys, _ := releasedDetachedTransportSeed(t, b, server, caps, 0)
	before := len(releasedDetachedTransportRequests(server))
	// Call the existing private dispatcher with the independently bounded
	// table to exercise its generic parser without enabling a v2 consumer.
	points, revision, err := b.readViewDispatch(context.Background(), keys, caps, nil)
	require.NoError(t, err)
	require.Positive(t, revision)
	require.Len(t, points, 13)
	require.Len(t, releasedDetachedTransportRequests(server), before+1)
	for i, point := range points {
		require.Equal(t, keys[i], string(point.Key))
		require.Len(t, point.Value, caps[i])
	}

	base := b.client.KV
	for _, fault := range []string{"recursive", "last-companion-over-cap", "leased-companion"} {
		t.Run(fault, func(t *testing.T) {
			b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool {
				return len(ops) == 13
			}, after: func(reply *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				switch fault {
				case "recursive":
					reply.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: (*pb.TxnResponse)(reply)}}
				case "last-companion-over-cap":
					reply.Responses[12].GetResponseRange().Kvs[0].Value = []byte(strings.Repeat("x", caps[12]+1))
				case "leased-companion":
					reply.Responses[12].GetResponseRange().Kvs[0].Lease = 1
				}
				return reply, nil
			}}
			t.Cleanup(func() { b.client.KV = base })
			_, _, err := b.readViewDispatch(context.Background(), keys, caps, nil)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
}

func TestReleasedDetachedTransportFullWireBudget(t *testing.T) {
	namespace, err := NewNamespace("/"+strings.Repeat("p", 253), strings.Repeat("s", 128), strings.Repeat("c", 127))
	require.NoError(t, err)
	require.Len(t, namespace.Root(), 512)
	identityKey, err := namespace.Key("meta", "identity")
	require.NoError(t, err)
	restoreKey, err := namespace.Key("meta", "restore_epoch")
	require.NoError(t, err)
	b := &Backend{
		namespace: namespace, identityKey: identityKey, restoreKey: restoreKey,
		identityValue: strings.Repeat("i", maxIdentityJSONBytes), restoreEpoch: strings.Repeat("e", 128),
	}
	profiles := []struct {
		name string
		caps []int
		pins int
	}{
		{"placement", []int{4096}, 18},
		{"current", []int{4096, 1024, 4096, 2048}, 18},
		{"intent", []int{4096}, 18},
		{"cohort", []int{4096, 4096, 2048, 2048}, 18},
		{"seal", []int{8192}, 18},
		{"termination", []int{16384, 16384}, 18},
		{"history", releasedDetachedTransportCaps(), 18},
		{"successor-final", []int{4096, 1024, 4096, 4096, 4096, 4096, 4096}, 22},
		// The alternative final uses the same eighth RPC, not a ninth call.
		{"final", []int{4096, 1024, 4096, 4096, 4096}, 22},
	}
	total := 0
	for _, profile := range profiles {
		keys := make([]string, len(profile.caps))
		pins := make([]clientv3.Cmp, profile.pins)
		maximumKey := func(kind string, ordinal int) string {
			prefix := fmt.Sprintf("%s%s-%02d-", namespace.Root(), kind, ordinal)
			return prefix + strings.Repeat("k", 1024-len(prefix))
		}
		for i := range keys {
			keys[i] = maximumKey("point", i)
			require.True(t, validNamespaceKey(namespace, keys[i]))
			require.Len(t, keys[i], 1024)
		}
		for i := range pins {
			key := maximumKey("pin", i)
			require.True(t, validNamespaceKey(namespace, key))
			pins[i] = clientv3.Compare(clientv3.ModRevision(key), "=", int64(math.MaxInt64))
		}
		metadata := []string{identityKey, restoreKey}
		request := &pb.TxnRequest{
			Compare: taskProtoComparisons(append(b.baseComparisons(), pins...)),
			Success: taskProtoPoints(keys), Failure: taskProtoPoints(metadata),
		}
		maximum := *request
		maximum.Compare = b.maximumCleanupBirthComparisons(request.Compare)
		replyCaps := append([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes}, profile.caps...)
		maximumReply := cleanupBirthPointReply(replyCaps)
		failure := cleanupBirthPointReply([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes})
		nodes := len(request.Compare) + len(request.Success) + len(request.Failure)
		require.LessOrEqual(t, nodes, 64)
		require.LessOrEqual(t, request.Size(), 128*1024)
		require.LessOrEqual(t, maximum.Size(), 128*1024)
		require.LessOrEqual(t, maximumReply.Size(), 256*1024)
		require.LessOrEqual(t, failure.Size(), 256*1024)
		require.GreaterOrEqual(t, maximumReply.Size(), failure.Size())
		require.NoError(t, taskCheckWire(namespace, []*pb.TxnRequest{request, &maximum}, []*pb.TxnResponse{maximumReply, failure}))
		if profile.name != "final" {
			total += maximumReply.Size()
		}
		t.Logf("transport-only %s: nodes=%d request=%d maximum_request=%d maximum_reply=%d failure=%d", profile.name, nodes, request.Size(), maximum.Size(), maximumReply.Size(), failure.Size())
	}
	require.LessOrEqual(t, total, 512*1024)
	t.Logf("transport-only detached full eight-RPC aggregate maximum=%d bytes", total)
	require.NoError(t, checkReleasedDetachedReadBudget())
	require.NoError(t, releasedDetachedReadBudget())
}

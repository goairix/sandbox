package etcd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func dueWatchCodecTestFrame(events int, keyBytes, valueBytes int) *pb.WatchResponse {
	r := &pb.WatchResponse{Header: &pb.ResponseHeader{ClusterId: 1, MemberId: 2, Revision: 100, RaftTerm: 1}, WatchId: 1}
	for i := 0; i < events; i++ {
		r.Events = append(r.Events, &mvccpb.Event{Kv: &mvccpb.KeyValue{Key: bytes.Repeat([]byte("k"), keyBytes), Value: bytes.Repeat([]byte("v"), valueBytes), CreateRevision: 90, ModRevision: 100, Version: 1}})
	}
	return r
}

func TestDueWatchCodecCanonicalAndPreallocation(t *testing.T) {
	valid := dueWatchCodecTestFrame(1, 64, 128)
	wire, err := valid.Marshal()
	require.NoError(t, err)
	var codec dueWatchCodec
	require.Equal(t, "proto", codec.Name())
	var got pb.WatchResponse
	require.NoError(t, codec.Unmarshal(wire, &got))
	require.Equal(t, valid, &got)
	request := &pb.WatchRequest{RequestUnion: &pb.WatchRequest_ProgressRequest{ProgressRequest: &pb.WatchProgressRequest{}}}
	encoded, err := codec.Marshal(request)
	require.NoError(t, err)
	var decoded pb.WatchRequest
	require.NoError(t, decoded.Unmarshal(encoded))
	require.Equal(t, request, &decoded)
	for name, build := range map[string]func() []byte{
		"tiny-event-amplification": func() []byte {
			r := dueWatchCodecTestFrame(0, 0, 0)
			b, _ := r.Marshal()
			for i := 0; i < 119; i++ {
				b = append(b, 0x5a, 0)
			}
			return b
		},
		"unknown-field":    func() []byte { return append(bytes.Clone(wire), 0xf8, 0x07, 0x01) },
		"duplicate-scalar": func() []byte { return append(bytes.Clone(wire), 0x10, 0x02) },
		"duplicate-header": func() []byte {
			h, _ := valid.Header.Marshal()
			return append(append(bytes.Clone(wire), 0x0a, byte(len(h))), h...)
		},
		"oversize-frame": func() []byte { return bytes.Repeat([]byte("x"), 262145) },
		"oversize-key":   func() []byte { r := dueWatchCodecTestFrame(1, 1025, 1); b, _ := r.Marshal(); return b },
		"oversize-value": func() []byte { r := dueWatchCodecTestFrame(1, 1, 1025); b, _ := r.Marshal(); return b },
		"unexpected-prev-kv": func() []byte {
			r := dueWatchCodecTestFrame(1, 1, 1)
			r.Events[0].PrevKv = r.Events[0].Kv
			b, _ := r.Marshal()
			return b
		},
		"truncated":            func() []byte { return bytes.Clone(wire[:len(wire)-1]) },
		"bad-nested-wire-type": func() []byte { return []byte{0x0a, 1, 0x0f} },
	} {
		t.Run(name, func(t *testing.T) {
			var codec dueWatchCodec
			target := &pb.WatchResponse{WatchId: 999}
			require.Error(t, codec.Unmarshal(build(), target))
			require.Equal(t, int64(999), target.WatchId, "reject before protobuf graph mutation")
			require.Nil(t, target.Events)
		})
	}
}

func TestDueWatchCodecAggregateLimits(t *testing.T) {
	t.Run("frames", func(t *testing.T) {
		var codec dueWatchCodec
		frame := dueWatchCodecTestFrame(0, 0, 0)
		frame.Created = true
		wire, err := frame.Marshal()
		require.NoError(t, err)
		for i := 0; i < 512; i++ {
			require.NoError(t, codec.Unmarshal(wire, new(pb.WatchResponse)))
		}
		target := &pb.WatchResponse{WatchId: 999}
		require.Error(t, codec.Unmarshal(wire, target))
		require.Equal(t, int64(999), target.WatchId)
	})
	t.Run("events", func(t *testing.T) {
		var codec dueWatchCodec
		remaining := 10000
		for remaining > 0 {
			count := min(118, remaining)
			wire, err := dueWatchCodecTestFrame(count, 1, 1).Marshal()
			require.NoError(t, err)
			require.NoError(t, codec.Unmarshal(wire, new(pb.WatchResponse)))
			remaining -= count
		}
		wire, err := dueWatchCodecTestFrame(1, 1, 1).Marshal()
		require.NoError(t, err)
		target := &pb.WatchResponse{WatchId: 999}
		require.Error(t, codec.Unmarshal(wire, target))
		require.Equal(t, int64(999), target.WatchId)
	})
	t.Run("bytes", func(t *testing.T) {
		var codec dueWatchCodec
		wire, err := dueWatchCodecTestFrame(118, 1024, 1024).Marshal()
		require.NoError(t, err)
		require.LessOrEqual(t, len(wire), 262144)
		for consumed := 0; consumed+len(wire) <= 8*1024*1024; consumed += len(wire) {
			require.NoError(t, codec.Unmarshal(wire, new(pb.WatchResponse)))
		}
		target := &pb.WatchResponse{WatchId: 999}
		require.Error(t, codec.Unmarshal(wire, target))
		require.Equal(t, int64(999), target.WatchId)
	})
}

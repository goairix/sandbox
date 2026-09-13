package redis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	redislib "github.com/redis/go-redis/v9"
)

// runSafetyScript preserves the Lua reply separately from post-write durability
// evidence. A failed WAIT is ambiguous: callers must retain returned identities
// for cleanup, but cannot treat the mutation as durably acknowledged.
func (s *Store) runSafetyScript(ctx context.Context, script *redislib.Script, keys []string, args ...any) (*redislib.Cmd, error) {
	if s.durability != DurabilityReplicaAck {
		return script.Run(ctx, s.client, keys, args...), nil
	}
	if len(keys) == 0 {
		cmd := redislib.NewCmd(ctx)
		cmd.SetErr(fmt.Errorf("safety script requires a routing key"))
		return cmd, nil
	}
	conn, err := s.safetyConnection(ctx, keys[0])
	if err != nil {
		cmd := redislib.NewCmd(ctx)
		cmd.SetErr(s.pendingSafetyFailure(ctx, err))
		return cmd, nil
	}
	defer func() { _ = conn.Close() }()
	cmd := script.Run(ctx, conn, keys, args...)
	if cmd.Err() != nil {
		cmd.SetErr(s.pendingSafetyFailure(ctx, cmd.Err()))
		return cmd, nil
	}
	return cmd, s.acknowledgePinnedWrite(ctx, conn, keys[0])
}

func (s *Store) runSafetyCommand(ctx context.Context, key string, run func(redislib.Cmdable) redislib.Cmder) (redislib.Cmder, error) {
	if s.durability != DurabilityReplicaAck {
		return run(s.client), nil
	}
	conn, err := s.safetyConnection(ctx, key)
	if err != nil {
		cmd := redislib.NewCmd(ctx)
		cmd.SetErr(s.pendingSafetyFailure(ctx, err))
		return cmd, nil
	}
	defer func() { _ = conn.Close() }()
	cmd := run(conn)
	if cmd.Err() != nil {
		cmd.SetErr(s.pendingSafetyFailure(ctx, cmd.Err()))
		return cmd, nil
	}
	return cmd, s.acknowledgePinnedWrite(ctx, conn, key)
}

func (s *Store) safetyConnection(ctx context.Context, key string) (*redislib.Conn, error) {
	if key == "" || strings.HasPrefix(key, "sandbox:durability-barrier:") {
		return nil, fmt.Errorf("invalid or reserved safety-write key")
	}
	switch client := s.client.(type) {
	case *redislib.Client: // Standalone and Sentinel both expose Client.Conn.
		return client.Conn(), nil
	case *redislib.ClusterClient:
		master, err := client.MasterForKey(ctx, key)
		if err != nil {
			return nil, err
		}
		return master.Conn(), nil
	default:
		return nil, fmt.Errorf("unsupported safety-write client %T", s.client)
	}
}

func (s *Store) acknowledgePinnedWrite(ctx context.Context, conn *redislib.Conn, key string) error {
	barrierKey, err := sameSlotSafetyBarrierKey(key)
	if err != nil {
		return errors.Join(state.ErrDurabilityUnconfirmed, err)
	}
	// A script may have returned success without writing (e.g. idempotent
	// deletion). WAIT on a fresh connection then has offset zero. This real
	// same-slot SET establishes an offset beyond the current master's prior
	// state, without touching any owner, lease, generation, or record key.
	if err := conn.Set(ctx, barrierKey, "1", time.Minute).Err(); err != nil {
		return errors.Join(state.ErrDurabilityUnconfirmed, s.pendingSafetyFailure(ctx, err))
	}
	// Round up: WAIT timeout=0 means an unbounded wait, not a sub-ms timeout.
	timeoutMillis, err := redisTTLMilliseconds(s.ackTimeout)
	if err != nil {
		return errors.Join(state.ErrDurabilityUnconfirmed, s.pendingSafetyFailure(ctx, err))
	}
	if timeoutMillis == 0 {
		timeoutMillis = 1
	}
	acknowledged, err := conn.Wait(ctx, s.ackReplicas, time.Duration(timeoutMillis)*time.Millisecond).Result()
	if err != nil {
		return errors.Join(state.ErrDurabilityUnconfirmed, s.pendingSafetyFailure(ctx, err))
	}
	if acknowledged < int64(s.ackReplicas) {
		return state.ErrDurabilityUnconfirmed
	}
	return nil
}

// A direct pinned master bypasses ClusterClient's retry/redirection machinery.
// Refresh topology after a failed attempt, but never replay this attempt on a
// different connection. ASK stays pending until slot migration is complete;
// there is intentionally no cross-connection ASKING/script/WAIT forwarding.
func (s *Store) pendingSafetyFailure(ctx context.Context, err error) error {
	if err == nil {
		return err
	}
	var networkErr net.Error
	if redislib.HasErrorPrefix(err, "MOVED") || redislib.HasErrorPrefix(err, "ASK") || redislib.HasErrorPrefix(err, "READONLY") || errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrClosedPipe) {
		if cluster, ok := s.client.(*redislib.ClusterClient); ok {
			cluster.ReloadState(ctx)
		}
		if s.durability == DurabilityReplicaAck {
			return errors.Join(state.ErrDurabilityUnconfirmed, err)
		}
	}
	return err
}

var safetySlotTags sync.Map // bounded by the 16384 Redis Cluster slots

func redisSafetyHashTag(key string) string {
	if start := strings.IndexByte(key, '{'); start >= 0 {
		if end := strings.IndexByte(key[start+1:], '}'); end > 0 {
			return key[start+1 : start+1+end]
		}
	}
	return key
}

func redisSafetySlot(key string) uint16 {
	var crc uint16
	for _, value := range []byte(redisSafetyHashTag(key)) {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc & 16383
}

func sameSlotSafetyBarrierKey(key string) (string, error) {
	tag := redisSafetyHashTag(key)
	if tag != "" && !strings.ContainsRune(tag, '}') {
		return "sandbox:durability-barrier:{" + tag + "}", nil
	}
	// Keys with no valid tag but literal closing braces cannot be wrapped as
	// tags. Find and cache a printable tag for their slot; work is bounded.
	slot := redisSafetySlot(key)
	if cached, ok := safetySlotTags.Load(slot); ok {
		return cached.(string), nil
	}
	for candidate := range 1 << 20 {
		tag := "safety-slot-" + strconv.Itoa(candidate)
		if redisSafetySlot(tag) == slot {
			barrier := "sandbox:durability-barrier:{" + tag + "}"
			actual, _ := safetySlotTags.LoadOrStore(slot, barrier)
			return actual.(string), nil
		}
	}
	return "", fmt.Errorf("cannot derive a same-slot durability barrier")
}

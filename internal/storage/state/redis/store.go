package redis

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/goairix/sandbox/internal/storage/state"
)

var compareAndSwapScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current or current ~= ARGV[1] then
    return 0
end
if tonumber(ARGV[3]) == 0 then
    redis.call('SET', KEYS[1], ARGV[2])
else
    redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
end
return 1
`)

var compareAndDeleteScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current or current ~= ARGV[1] then
    return 0
end
redis.call('DEL', KEYS[1])
return 1
`)

var compareAndDeleteIfAbsentScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then return 0 end
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1])
return 1
`)

var confirmAbsenceScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 1 end
return 0
`)

type Options struct {
	Mode         Mode
	Addr         string
	Addrs        []string
	MasterName   string
	Username     string
	Password     string
	DB           int
	Durability   DurabilityMode
	AckReplicas  int
	AckTimeout   time.Duration
	PoolSize     int
	MinIdleConns int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxRetries   int
}

type Store struct {
	client      redis.UniversalClient
	durability  DurabilityMode
	ackReplicas int
	ackTimeout  time.Duration
}

type Mode string

const (
	ModeStandalone Mode = "standalone"
	ModeSentinel   Mode = "sentinel"
	ModeCluster    Mode = "cluster"
)

type DurabilityMode string

const (
	DurabilityBestEffort DurabilityMode = "best_effort"
	DurabilityNative     DurabilityMode = "native"
	DurabilityReplicaAck DurabilityMode = "replica_ack"
)

func New(ctx context.Context, opts Options) (*Store, error) {
	mode := opts.Mode
	if mode == "" {
		mode = ModeStandalone
	}
	addrs := append([]string(nil), opts.Addrs...)
	if len(addrs) == 0 && opts.Addr != "" {
		addrs = []string{opts.Addr}
	}
	if len(addrs) == 0 {
		return nil, errors.New("redis: at least one address is required")
	}
	switch mode {
	case ModeStandalone:
		if len(addrs) != 1 {
			return nil, errors.New("redis: standalone mode requires exactly one address")
		}
	case ModeSentinel:
		if opts.MasterName == "" {
			return nil, errors.New("redis: sentinel mode requires a master name")
		}
	case ModeCluster:
		if opts.DB != 0 {
			return nil, errors.New("redis: cluster mode supports database 0 only")
		}
	default:
		return nil, fmt.Errorf("redis: unsupported mode %q", mode)
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: addrs, MasterName: opts.MasterName, Username: opts.Username,
		Password: opts.Password, DB: opts.DB, PoolSize: opts.PoolSize,
		MinIdleConns: opts.MinIdleConns, DialTimeout: opts.DialTimeout,
		ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout,
		MaxRetries: opts.MaxRetries,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis: ping failed: %w", err)
	}
	durability := opts.Durability
	if durability == "" {
		durability = DurabilityBestEffort
	}
	ackReplicas := opts.AckReplicas
	if ackReplicas <= 0 {
		ackReplicas = 1
	}
	ackTimeout := opts.AckTimeout
	if ackTimeout <= 0 {
		ackTimeout = 100 * time.Millisecond
	}
	return &Store{client: client, durability: durability, ackReplicas: ackReplicas, ackTimeout: ackTimeout}, nil
}

func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	cmd, durabilityErr := s.runSafetyCommand(ctx, key, func(client redis.Cmdable) redis.Cmder {
		return client.Set(ctx, key, value, ttl)
	})
	return errors.Join(cmd.Err(), durabilityErr)
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := s.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	return val, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	cmd, durabilityErr := s.runSafetyCommand(ctx, key, func(client redis.Cmdable) redis.Cmder {
		return client.Del(ctx, key)
	})
	return errors.Join(cmd.Err(), durabilityErr)
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	n, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	cmd, durabilityErr := s.runSafetyCommand(ctx, key, func(client redis.Cmdable) redis.Cmder {
		return client.SetNX(ctx, key, value, ttl)
	})
	if cmd.Err() != nil {
		return false, cmd.Err()
	}
	created, err := cmd.(*redis.BoolCmd).Result()
	if err != nil {
		return created, err
	}
	if durabilityErr != nil {
		return false, durabilityErr
	}
	return created, nil
}

func (s *Store) Keys(ctx context.Context, pattern string) ([]string, error) {
	if cluster, ok := s.client.(*redis.ClusterClient); ok {
		var mu sync.Mutex
		seen := make(map[string]struct{})
		err := cluster.ForEachMaster(ctx, func(ctx context.Context, master *redis.Client) error {
			iter := master.Scan(ctx, 0, pattern, 0).Iterator()
			for iter.Next(ctx) {
				mu.Lock()
				seen[iter.Val()] = struct{}{}
				mu.Unlock()
			}
			return iter.Err()
		})
		if err != nil {
			return nil, s.pendingSafetyFailure(ctx, err)
		}
		keys := make([]string, 0, len(seen))
		for key := range seen {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys, nil
	}
	var keys []string
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// ConfirmAbsence never deletes an empty/corrupt value. In replica_ack mode the
// read-only predicate is followed by a real same-slot replication barrier.
func (s *Store) ConfirmAbsence(ctx context.Context, key string) (bool, error) {
	cmd, durabilityErr := s.runSafetyScript(ctx, confirmAbsenceScript, []string{key})
	result, err := cmd.Int64()
	if err != nil {
		return false, err
	}
	if durabilityErr != nil {
		return false, durabilityErr
	}
	return result == 1, nil
}

func (s *Store) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	ttlMillis, err := redisTTLMilliseconds(ttl)
	if err != nil {
		return false, err
	}
	cmd, durabilityErr := s.runSafetyScript(ctx, compareAndSwapScript, []string{key}, oldValue, newValue, ttlMillis)
	result, err := cmd.Int64()
	if err != nil {
		return false, err
	}
	if durabilityErr != nil {
		return false, durabilityErr
	}
	return result == 1, nil
}

func (s *Store) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	cmd, durabilityErr := s.runSafetyScript(ctx, compareAndDeleteScript, []string{key}, expected)
	result, err := cmd.Int64()
	if err != nil {
		return false, err
	}
	if durabilityErr != nil {
		return false, durabilityErr
	}
	return result == 1, nil
}

// CompareAndDeleteIfAbsent atomically fences owner recovery against a renewed
// lease. Both keys must share a Redis Cluster hash tag.
func (s *Store) CompareAndDeleteIfAbsent(ctx context.Context, key string, expected []byte, absentKey string) (bool, error) {
	cmd, durabilityErr := s.runSafetyScript(ctx, compareAndDeleteIfAbsentScript, []string{key, absentKey}, expected)
	result, err := cmd.Int64()
	if err != nil {
		return false, err
	}
	if durabilityErr != nil {
		return false, durabilityErr
	}
	return result == 1, nil
}

func (s *Store) Increment(ctx context.Context, key string) (int64, error) {
	cmd, durabilityErr := s.runSafetyCommand(ctx, key, func(client redis.Cmdable) redis.Cmder {
		return client.Incr(ctx, key)
	})
	err := cmd.Err()
	if redis.HasErrorPrefix(err, "increment or decrement would overflow") {
		return 0, state.ErrIncrementOverflow
	}
	if err != nil {
		return 0, err
	}
	value, err := cmd.(*redis.IntCmd).Result()
	return value, errors.Join(err, durabilityErr)
}

func redisTTLMilliseconds(ttl time.Duration) (int64, error) {
	if ttl < 0 {
		return 0, errors.New("redis: TTL must not be negative")
	}
	if ttl == 0 {
		return 0, nil
	}
	millis := ttl / time.Millisecond
	if ttl%time.Millisecond != 0 {
		millis++
	}
	return int64(millis), nil
}

// Close closes the Redis connection.
func (s *Store) Close() error {
	return s.client.Close()
}

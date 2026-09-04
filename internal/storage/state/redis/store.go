package redis

import (
	"context"
	"errors"
	"fmt"
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

type Options struct {
	Addr     string
	Password string
	DB       int
}

type Store struct {
	client *redis.Client
}

func New(ctx context.Context, opts Options) (*Store, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     opts.Addr,
		Password: opts.Password,
		DB:       opts.DB,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis: ping failed: %w", err)
	}
	return &Store{client: client}, nil
}

func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
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
	return s.client.Del(ctx, key).Err()
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	n, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return s.client.SetNX(ctx, key, value, ttl).Result()
}

func (s *Store) Keys(ctx context.Context, pattern string) ([]string, error) {
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

func (s *Store) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	ttlMillis, err := redisTTLMilliseconds(ttl)
	if err != nil {
		return false, err
	}
	result, err := compareAndSwapScript.Run(ctx, s.client, []string{key}, oldValue, newValue, ttlMillis).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (s *Store) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	result, err := compareAndDeleteScript.Run(ctx, s.client, []string{key}, expected).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (s *Store) Increment(ctx context.Context, key string) (int64, error) {
	value, err := s.client.Incr(ctx, key).Result()
	if redis.HasErrorPrefix(err, "increment or decrement would overflow") {
		return 0, state.ErrIncrementOverflow
	}
	return value, err
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

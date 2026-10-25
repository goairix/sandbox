// Package etcd implements the sandbox state contracts on an etcd namespace.
package etcd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	errInvalidKey = errors.New("etcd state: key must not be empty")
	errInvalidTTL = errors.New("etcd state: TTL must not be negative or overflow lease seconds")
)

// Options identifies one isolated state namespace and bounds each operation.
// TLS may be nil for a loopback test server; production wiring must supply it.
type Options struct {
	Endpoints []string
	Prefix    string
	Timeout   time.Duration
	TLS       *tls.Config

	// ControllerStageObserver receives only fixed operation, stage, and result
	// labels. It must not block controller RPCs or record sandbox identifiers.
	ControllerStageObserver func(context.Context, string, string, string, time.Duration)
}

// Store owns an etcd client and implements the ordinary atomic state contract.
// Each method uses linearizable reads or a single etcd transaction; errors on
// writes may mean that the server committed after the caller lost its reply.
type Store struct {
	client                  *clientv3.Client
	prefix                  string
	timeout                 time.Duration
	controllerStageObserver func(context.Context, string, string, string, time.Duration)
}

var _ state.AtomicStore = (*Store)(nil)

// New opens a client for an absolute, slash-terminated namespace prefix.
func New(ctx context.Context, options Options) (*Store, error) {
	if len(options.Endpoints) == 0 || options.Timeout <= 0 {
		return nil, errors.New("etcd state: endpoints and positive timeout are required")
	}
	if !strings.HasPrefix(options.Prefix, "/") || !strings.HasSuffix(options.Prefix, "/") || options.Prefix == "/" {
		return nil, errors.New("etcd state: prefix must be an absolute namespace ending in a slash")
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   append([]string(nil), options.Endpoints...),
		DialTimeout: options.Timeout,
		TLS:         options.TLS,
	})
	if err != nil {
		return nil, err
	}
	s := &Store{
		client: client, prefix: options.Prefix, timeout: options.Timeout,
		controllerStageObserver: options.ControllerStageObserver,
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	if _, err := client.Get(requestCtx, options.Prefix, clientv3.WithCountOnly()); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("etcd state: initial read: %w", err)
	}
	return s, nil
}

// Close releases the client connection. It does not revoke record leases.
func (s *Store) Close() error {
	return s.client.Close()
}

func (s *Store) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout)
}

func (s *Store) key(key string) (string, error) {
	if key == "" {
		return "", errInvalidKey
	}
	return s.prefix + key, nil
}

// leaseFor grants a lease before a write. etcd leases have whole-second
// granularity; round upward so a session cannot disappear early. This can
// retain a value for less than one extra second. A failed or unknown write leaves an
// unattached lease to expire naturally: revoking it after an ambiguous reply
// could delete a committed value. Only a known failed compare may revoke it.
func (s *Store) leaseFor(ctx context.Context, ttl time.Duration) (clientv3.LeaseID, error) {
	if ttl == 0 {
		return 0, nil
	}
	if ttl < 0 {
		return 0, errInvalidTTL
	}
	seconds := ttl / time.Second
	if ttl%time.Second != 0 {
		seconds++
	}
	if seconds > math.MaxInt64/time.Second {
		return 0, errInvalidTTL
	}
	response, err := s.client.Grant(ctx, int64(seconds))
	if err != nil {
		return 0, err
	}
	return response.ID, nil
}

func putOptions(lease clientv3.LeaseID) []clientv3.OpOption {
	if lease == 0 {
		return nil
	}
	return []clientv3.OpOption{clientv3.WithLease(lease)}
}

// Set replaces one value; TTL zero makes it persistent.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	fullKey, err := s.key(key)
	if err != nil {
		return err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	lease, err := s.leaseFor(requestCtx, ttl)
	if err != nil {
		return err
	}
	_, err = s.client.Put(requestCtx, fullKey, string(value), putOptions(lease)...)
	return err
}

// Get returns nil for a missing key and a copy of its original bytes otherwise.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	fullKey, err := s.key(key)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	response, err := s.client.Get(requestCtx, fullKey)
	if err != nil || len(response.Kvs) == 0 {
		return nil, err
	}
	return append([]byte{}, response.Kvs[0].Value...), nil
}

// Delete removes a key if present.
func (s *Store) Delete(ctx context.Context, key string) error {
	fullKey, err := s.key(key)
	if err != nil {
		return err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	_, err = s.client.Delete(requestCtx, fullKey)
	return err
}

// Exists performs a linearizable presence read.
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	fullKey, err := s.key(key)
	if err != nil {
		return false, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	response, err := s.client.Get(requestCtx, fullKey, clientv3.WithCountOnly())
	if err != nil {
		return false, err
	}
	return response.Count != 0, nil
}

// ConfirmAbsence confirms absence with a linearizable read.
func (s *Store) ConfirmAbsence(ctx context.Context, key string) (bool, error) {
	exists, err := s.Exists(ctx, key)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// SetNX stores a value only while its key is absent. A transaction error has
// an unknown outcome; callers must query before deciding to retry.
func (s *Store) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	fullKey, err := s.key(key)
	if err != nil {
		return false, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	lease, err := s.leaseFor(requestCtx, ttl)
	if err != nil {
		return false, err
	}
	response, err := s.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.Version(fullKey), "=", 0)).
		Then(clientv3.OpPut(fullKey, string(value), putOptions(lease)...)).Commit()
	if err != nil {
		return false, err
	}
	if !response.Succeeded && lease != 0 {
		// The failed compare proves this lease was never attached to this key.
		_, _ = s.client.Revoke(requestCtx, lease)
	}
	return response.Succeeded, nil
}

// CompareAndSwap replaces a present value only when its bytes match.
func (s *Store) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	fullKey, err := s.key(key)
	if err != nil {
		return false, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	lease, err := s.leaseFor(requestCtx, ttl)
	if err != nil {
		return false, err
	}
	response, err := s.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.Version(fullKey), ">", 0), clientv3.Compare(clientv3.Value(fullKey), "=", string(oldValue))).
		Then(clientv3.OpPut(fullKey, string(newValue), putOptions(lease)...)).Commit()
	if err != nil {
		return false, err
	}
	if !response.Succeeded && lease != 0 {
		_, _ = s.client.Revoke(requestCtx, lease)
	}
	return response.Succeeded, nil
}

// CompareAndDelete removes a present value only when its bytes match.
func (s *Store) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	fullKey, err := s.key(key)
	if err != nil {
		return false, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	response, err := s.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.Version(fullKey), ">", 0), clientv3.Compare(clientv3.Value(fullKey), "=", string(expected))).
		Then(clientv3.OpDelete(fullKey)).Commit()
	if err != nil {
		return false, err
	}
	return response.Succeeded, nil
}

// CompareAndDeleteIfAbsent fences owner removal against a concurrent lease.
func (s *Store) CompareAndDeleteIfAbsent(ctx context.Context, key string, expected []byte, absentKey string) (bool, error) {
	fullKey, err := s.key(key)
	if err != nil {
		return false, err
	}
	absentFullKey, err := s.key(absentKey)
	if err != nil {
		return false, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	response, err := s.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.Version(fullKey), ">", 0),
			clientv3.Compare(clientv3.Value(fullKey), "=", string(expected)),
			clientv3.Compare(clientv3.Version(absentFullKey), "=", 0)).
		Then(clientv3.OpDelete(fullKey)).Commit()
	if err != nil {
		return false, err
	}
	return response.Succeeded, nil
}

// Increment updates a persistent signed integer with a revision-fenced CAS.
// A transport error never triggers an automatic retry because the previous
// transaction may have committed without an ACK.
func (s *Store) Increment(ctx context.Context, key string) (int64, error) {
	fullKey, err := s.key(key)
	if err != nil {
		return 0, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	for {
		read, err := s.client.Get(requestCtx, fullKey)
		if err != nil {
			return 0, err
		}
		var current int64
		compare := clientv3.Compare(clientv3.Version(fullKey), "=", 0)
		var lease clientv3.LeaseID
		if len(read.Kvs) != 0 {
			current, err = strconv.ParseInt(string(read.Kvs[0].Value), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("etcd state: invalid integer: %w", err)
			}
			compare = clientv3.Compare(clientv3.ModRevision(fullKey), "=", read.Kvs[0].ModRevision)
			lease = clientv3.LeaseID(read.Kvs[0].Lease)
		}
		if current == math.MaxInt64 {
			return 0, state.ErrIncrementOverflow
		}
		next := current + 1
		// Redis INCR preserves the existing TTL. Reuse the observed lease;
		// the revision compare rejects a concurrent replacement or expiry.
		written, err := s.client.Txn(requestCtx).If(compare).
			Then(clientv3.OpPut(fullKey, strconv.FormatInt(next, 10), putOptions(lease)...)).Commit()
		if err != nil {
			return 0, err
		}
		if written.Succeeded {
			return next, nil
		}
	}
}

// Keys lists a strict trailing-star prefix in bounded etcd pages. The result
// still materializes all matching keys, so production callers must avoid this
// method for high-cardinality audit paths until their traversal is paged.
func (s *Store) Keys(ctx context.Context, pattern string) ([]string, error) {
	if !strings.HasSuffix(pattern, "*") || strings.Count(pattern, "*") != 1 || strings.ContainsAny(pattern, "?[") || len(pattern) == 1 {
		return nil, fmt.Errorf("etcd state: unsupported key pattern %q", pattern)
	}
	prefix := s.prefix + strings.TrimSuffix(pattern, "*")
	end := clientv3.GetPrefixRangeEnd(prefix)
	start := prefix
	var revision int64
	var keys []string
	for {
		requestCtx, cancel := s.requestContext(ctx)
		options := []clientv3.OpOption{clientv3.WithRange(end), clientv3.WithKeysOnly(), clientv3.WithLimit(256)}
		if revision != 0 {
			options = append(options, clientv3.WithRev(revision))
		}
		response, err := s.client.Get(requestCtx, start, options...)
		cancel()
		if err != nil {
			return nil, err
		}
		if revision == 0 {
			// A single revision excludes insertions between pages. Compaction
			// fails the scan instead of silently returning a mixed snapshot.
			revision = response.Header.Revision
		}
		for _, kv := range response.Kvs {
			keys = append(keys, strings.TrimPrefix(string(kv.Key), s.prefix))
		}
		if !response.More {
			return keys, nil
		}
		start = string(response.Kvs[len(response.Kvs)-1].Key) + "\x00"
	}
}

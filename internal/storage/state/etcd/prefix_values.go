package etcd

import (
	"context"
	"fmt"
	"strings"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// VisitPrefixValues reads key/value pages from one etcd revision. A compaction
// or unreadable page aborts the visit; callers must not interpret that as an
// empty owner set. Returning stop=true from visit ends the traversal early.
func (s *Store) VisitPrefixValues(ctx context.Context, prefix string, visit func(string, []byte) (stop bool, err error)) error {
	fullPrefix, err := s.key(prefix)
	if err != nil || visit == nil {
		return fmt.Errorf("etcd state: invalid prefix value visitor")
	}
	start := fullPrefix
	end := clientv3.GetPrefixRangeEnd(fullPrefix)
	var revision int64
	for {
		requestCtx, cancel := s.requestContext(ctx)
		// Owner records may each be 128 KiB. Keep one response below a few
		// MiB even when every entry is at the accepted record-size bound.
		options := []clientv3.OpOption{clientv3.WithRange(end), clientv3.WithLimit(16)}
		if revision != 0 {
			options = append(options, clientv3.WithRev(revision))
		}
		response, readErr := s.client.Get(requestCtx, start, options...)
		cancel()
		if readErr != nil {
			return readErr
		}
		if revision == 0 {
			revision = response.Header.Revision
		}
		for _, kv := range response.Kvs {
			key := string(kv.Key)
			if !strings.HasPrefix(key, fullPrefix) {
				return fmt.Errorf("etcd state: prefix scan escaped namespace")
			}
			stop, visitErr := visit(strings.TrimPrefix(key, s.prefix), kv.Value)
			if visitErr != nil {
				return visitErr
			}
			if stop {
				return nil
			}
		}
		if !response.More {
			return nil
		}
		if len(response.Kvs) == 0 {
			return fmt.Errorf("etcd state: prefix scan did not advance")
		}
		start = string(response.Kvs[len(response.Kvs)-1].Key) + "\x00"
	}
}

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This benchmarks Manager admission against a real three-member fixture. The
// runtime is controlled in-process, so these are coordination costs, not HTTP
// or Kubernetes exec latency claims.
func BenchmarkSharedDataCoordination(b *testing.B) {
	for _, scenario := range []string{"one_hot", "many_cold", "long_stream", "mixed_replicas"} {
		for _, capable := range []bool{false, true} {
			name := "legacy"
			if capable {
				name = "shared"
			}
			b.Run(scenario+"/"+name, func(b *testing.B) {
				capabilities := []bool{capable}
				if scenario == "mixed_replicas" {
					capabilities = append(capabilities, capable)
				}
				f := newCoordinationEtcdFixture(b, capabilities...)
				ids := []string{"s"}
				if scenario == "many_cold" {
					for i := 1; i < 2048; i++ {
						id := fmt.Sprintf("s-%d", i)
						f.publish(b, id)
						ids = append(ids, id)
					}
				}
				// Warm only the known hot participants. The cold/high-cardinality case
				// includes first registrations and the 1024-entry capacity fallback.
				if scenario != "many_cold" {
					for _, m := range f.managers {
						_, _, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
						if err != nil {
							b.Fatal(err)
						}
						release()
					}
				}
				before := f.costs.snapshot()
				eventsBefore, bytesBefore := f.costs.watchEvents.Load(), f.costs.watchBytes.Load()
				statusBefore, err := f.client.Status(context.Background(), coordinationEndpoints(b)[0])
				if err != nil {
					b.Fatal(err)
				}
				var next atomic.Int64
				var inflight, peak atomic.Int64
				var samplesMu sync.Mutex
				samples := make([]int64, 0, b.N)
				failures := make(chan error, 64)
				var wg sync.WaitGroup
				runStarted := time.Now()
				b.ResetTimer()
				for range 64 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						local := make([]int64, 0, b.N/64+1)
						for {
							i := next.Add(1) - 1
							if i >= int64(b.N) {
								break
							}
							target := runStarted.Add(time.Duration(i) * time.Second / 160)
							if delay := time.Until(target); delay > 0 {
								timer := time.NewTimer(delay)
								<-timer.C
							}
							active := inflight.Add(1)
							for old := peak.Load(); active > old; old = peak.Load() {
								if peak.CompareAndSwap(old, active) {
									break
								}
							}
							m := f.managers[int(i)%len(f.managers)]
							started := time.Now()
							_, _, release, err := m.beginDistributedOperation(context.Background(), ids[int(i)%len(ids)], state.ActiveOperationData)
							if err != nil {
								failures <- err
								break
							}
							if scenario == "long_stream" {
								timer := time.NewTimer(500 * time.Millisecond)
								<-timer.C
							}
							release()
							inflight.Add(-1)
							local = append(local, time.Since(started).Nanoseconds())
						}
						samplesMu.Lock()
						samples = append(samples, local...)
						samplesMu.Unlock()
					}()
				}
				wg.Wait()
				b.StopTimer()
				close(failures)
				for err := range failures {
					b.Fatal(err)
				}
				after := f.costs.snapshot()
				stages := map[string]int64{}
				var total, writes, grants, keepalives int64
				for key, n := range after {
					n -= before[key]
					stages[key] = n
					total += n
					if key == "open/commit/success" || key == "end/commit/success" {
						writes += n
					}
					if key == "open/grant/success" {
						grants += n
					}
					if key == "renew/keepalive/success" {
						keepalives += n
					}
				}
				b.ReportMetric(float64(total)/float64(b.N), "rpc/op")
				b.ReportMetric(float64(writes)/float64(b.N), "write-txn/op")
				b.ReportMetric(float64(grants)/float64(b.N), "grant/op")
				b.ReportMetric(float64(keepalives)/float64(b.N), "keepalive/op")
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				percentile := func(p float64) float64 {
					if len(samples) == 0 {
						return 0
					}
					return float64(samples[min(len(samples)-1, int(float64(len(samples))*p))]) / 1e6
				}
				b.ReportMetric(percentile(.50), "p50-ms")
				b.ReportMetric(percentile(.95), "p95-ms")
				b.ReportMetric(percentile(.99), "p99-ms")
				statusAfter, err := f.client.Status(context.Background(), coordinationEndpoints(b)[0])
				if err != nil {
					b.Fatal(err)
				}
				keys, err := f.client.Get(context.Background(), f.prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
				if err != nil {
					b.Fatal(err)
				}
				leases, err := f.client.Leases(context.Background())
				if err != nil {
					b.Fatal(err)
				}
				groups := 0
				for _, m := range f.managers {
					if m.sharedData != nil {
						m.sharedData.mu.Lock()
						groups += len(m.sharedData.groups)
						m.sharedData.mu.Unlock()
					}
				}
				detail := map[string]any{"scenario": scenario, "shared": capable, "operations": b.N, "concurrency_limit": 64, "offered_rate": 160, "inflight_peak": peak.Load(), "elapsed_seconds": time.Since(runStarted).Seconds(), "managers": len(f.managers), "stages": stages, "watch_events": f.costs.watchEvents.Load() - eventsBefore, "watch_event_bytes": f.costs.watchBytes.Load() - bytesBefore, "groups_end": groups, "keys_end": keys.Count, "cluster_leases_end": len(leases.Leases), "db_size_delta": statusAfter.DbSize - statusBefore.DbSize, "db_inuse_delta": statusAfter.DbSizeInUse - statusBefore.DbSizeInUse}
				encoded, _ := json.Marshal(detail)
				b.Logf("COORDINATION_COSTS %s", encoded)
			})
		}
	}
}

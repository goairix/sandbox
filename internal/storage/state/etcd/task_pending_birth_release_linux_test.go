//go:build linux

package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/client-go/rest"
)

// This negative-fixture issuer can never produce permission or a proof. Every
// invocation fails closed and is counted; metadata diagnosis must invoke none.
type cleanupBirthReleaseNegativeIssuer struct{ calls atomic.Int32 }

func (i *cleanupBirthReleaseNegativeIssuer) Certificate(context.Context) ([]byte, error) {
	i.calls.Add(1)
	return nil, errors.New("negative release fixture cannot issue certificates")
}

func (i *cleanupBirthReleaseNegativeIssuer) SignRuntimeTermination(context.Context, string, p.TaskRuntimeTerminationTicketClaims) ([]byte, error) {
	i.calls.Add(1)
	return nil, errors.New("negative release fixture cannot sign termination")
}

// Configure all dependencies before New. The real Linux recovery store owns a
// private temporary directory, and the concrete provider uses verified TLS but
// receives no request. The gRPC model proves metadata/lifecycle behavior only.
func newCleanupBirthReleaseConfiguredFixture(t *testing.T) *backfillBootstrapFixture {
	t.Helper()
	server, options := workerTestEndpoint(t)
	options.WorkerControl.SessionTTL = 60 * time.Second
	root, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "release-model", Roots: []ed25519.PublicKey{root}}
	options.Clock = goodAuthorityClock()
	options.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 1}
	issuer := &cleanupBirthReleaseNegativeIssuer{}
	options.TaskRuntimeTerminationIssuer = issuer

	var requests atomic.Int32
	providerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "metadata fixture forbids provider calls", http.StatusForbidden)
	}))
	t.Cleanup(providerServer.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: providerServer.Certificate().Raw})
	provider, err := k.NewTaskRuntimeCleanupClient(k.TaskRuntimeCleanupOptions{
		Config:    &rest.Config{Host: providerServer.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca}},
		Namespace: "release-model", RequestTimeout: time.Second, MaxActive: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })
	options.LocalRuntimeRelease = &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: 10 * time.Second, Provider: provider}
	recovery := recoveryTestOptions(archiveTestDirectory(t), 1)
	options.TaskWorkspaceArchiveRecovery = &recovery
	budget := &TaskWorkspaceSyncBudgetOptions{BudgetID: taskClaimTestID, SlotsPerPartition: 1, MaxIntentBytes: 4096}
	options.TaskWorkspaceSyncRecyclingBudget = budget
	key, err := options.Namespace.Key("meta", "workspace_sync_recycling_budget")
	require.NoError(t, err)
	wire, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{2, *budget}, 1024)
	require.NoError(t, err)
	server.put(key, wire, 0)

	b, err := New(context.Background(), options)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, b.Close())
		require.Zero(t, issuer.calls.Load())
		require.Zero(t, requests.Load())
	})
	session, err := b.OpenWorkerSession(context.Background(), "release-model-worker")
	require.NoError(t, err)
	partition, err := b.ClaimWorkerPartition(context.Background(), session, 0xaa)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	return &backfillBootstrapFixture{b: b, server: server, session: session, partition: partition, ranges: ranges, claims: claims}
}

func cleanupBirthReleaseHistoryOps(ops []clientv3.Op) bool {
	return len(ops) == 4 && ops[0].IsGet()
}

func TestCleanupBirthReleaseConfigured(t *testing.T) {
	t.Run("original-evidence-and-budget-admission", func(t *testing.T) {
		f := newCleanupBirthReleaseConfiguredFixture(t)
		birth := cleanupBirthReleaseTestObservation(t, f)
		other := newBackfillBootstrapFixture(t, 60*time.Second)
		foreign := cleanupBirthReleaseTestObservation(t, other)
		copied, copiedPartition := *birth, *f.partition
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		before := len(backfillBootstrapRequests(f))
		for _, tc := range []struct {
			name string
			p    *WorkerPartitionClaim
			r    *WorkerBudgetShare
			b    *cleanupBirthEvidence
		}{
			{"nil-birth", f.partition, f.ranges, nil},
			{"copied-birth", f.partition, f.ranges, &copied},
			{"foreign-birth", f.partition, f.ranges, foreign},
			{"copied-partition", &copiedPartition, f.ranges, birth},
			{"foreign-range", f.partition, other.ranges, birth},
			{"claim-budget", f.partition, f.claims, birth},
		} {
			t.Run(tc.name, func(t *testing.T) {
				proof, err := f.b.readCleanupBirthRelease(context.Background(), tc.p, tc.r, tc.b)
				require.Error(t, err)
				require.Nil(t, proof)
			})
		}
		require.Len(t, backfillBootstrapRequests(f), before)
		f.b.archiveLife.mu.Lock()
		tokens, liveErr := f.ranges.bucket.tokens, f.session.live()
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, uint64(262144), tokens)
		require.NoError(t, liveErr)

		backfillBootstrapCredit(f.b, f.ranges, 0)
		proof, err := f.b.readCleanupBirthRelease(context.Background(), f.partition, f.ranges, birth)
		require.ErrorIs(t, err, ErrConflict)
		require.Nil(t, proof)
		require.Len(t, backfillBootstrapRequests(f), before)
	})
	for _, candidate := range []string{"unsealed-history", "release-removed"} {
		t.Run(candidate+"-postguard-preserves-worker", func(t *testing.T) {
			f := newCleanupBirthReleaseConfiguredFixture(t)
			birth := cleanupBirthReleaseTestObservation(t, f)
			wantNodes := []int{25, 10, 12, 25}
			if candidate == "release-removed" {
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(birth.release.key)).Commit()
				require.NoError(t, err)
				wantNodes = []int{25, 10, 25}
			}
			before := len(backfillBootstrapRequests(f))
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			proof, err := f.b.readCleanupBirthRelease(context.Background(), f.partition, f.ranges, birth)
			require.Error(t, err)
			require.Nil(t, proof, "unsealed or missing evidence never yields a signed proof")
			requests := backfillBootstrapRequests(f)[before:]
			require.Len(t, requests, len(wantNodes))
			for i, request := range requests {
				require.Equal(t, wantNodes[i], len(request.Compare)+len(request.Success)+len(request.Failure))
			}
			f.b.archiveLife.mu.Lock()
			tokens, held, liveErr := f.ranges.bucket.tokens, f.ranges.dueOwner != nil, f.session.live()
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens)
			require.False(t, held)
			require.NoError(t, liveErr, "actual successful postguard permits fair later candidates")
		})
	}
	for _, failure := range []string{"joined-transport", "guard-changed", "caller-canceled"} {
		t.Run(failure+"-loses-original-session", func(t *testing.T) {
			f := newCleanupBirthReleaseConfiguredFixture(t)
			birth := cleanupBirthReleaseTestObservation(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			cause := &cleanupBirthReleaseTransportCause{message: "history transport failed"}
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthReleaseHistoryOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err == nil {
					switch failure {
					case "joined-transport":
						return nil, cause
					case "guard-changed":
						f.server.put(f.partition.fence.key, f.partition.fence.value, f.partition.fence.lease)
					case "caller-canceled":
						cancel()
					}
				}
				return r, err
			}}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			proof, err := f.b.readCleanupBirthRelease(ctx, f.partition, f.ranges, birth)
			require.Error(t, err)
			require.Nil(t, proof)
			if failure == "joined-transport" {
				require.ErrorIs(t, err, cause)
				require.ErrorIs(t, err, ErrCorruptRecord, "original private loader joins missing history with its transport error")
				var marker *enrolledPointTransportError
				require.ErrorAs(t, err, &marker)
			}
			f.b.archiveLife.mu.Lock()
			tokens, liveErr := f.ranges.bucket.tokens, f.session.live()
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens)
			require.Error(t, liveErr)
		})
	}
	for _, action := range []string{"Close", "caller-deadline"} {
		t.Run(action+"-joins-actual-history-return", func(t *testing.T) {
			f := newCleanupBirthReleaseConfiguredFixture(t)
			birth := cleanupBirthReleaseTestObservation(t, f)
			relist, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRelist, 0)
			require.NoError(t, err)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthReleaseHistoryOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				entered <- struct{}{}
				<-release
				return r, err
			}}
			ctx := context.Background()
			if action == "caller-deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
				t.Cleanup(cancel)
			}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			done, closed := make(chan error, 1), make(chan error, 1)
			started := time.Now()
			go func() {
				done <- f.b.RunWorkerRelist(ctx, f.partition, relist, func(owned context.Context) error {
					_, err := f.b.readCleanupBirthRelease(owned, f.partition, f.ranges, birth)
					return err
				})
			}()
			workerTestReceive(t, entered)
			f.b.archiveLife.mu.Lock()
			phaseContext := f.ranges.dueOwner.call.ctx
			deadline, bounded := phaseContext.Deadline()
			f.b.archiveLife.mu.Unlock()
			require.True(t, bounded)
			require.LessOrEqual(t, deadline.Sub(started), 5*f.b.requestTimeout+time.Second+100*time.Millisecond)
			if action == "Close" {
				go func() { closed <- f.b.Close() }()
			}
			workerTestReceive(t, phaseContext.Done())
			f.b.archiveLife.mu.Lock()
			pageHeld, relistHeld := f.ranges.dueOwner != nil, relist.owner != nil
			f.b.archiveLife.mu.Unlock()
			require.True(t, pageHeld)
			require.True(t, relistHeld)
			select {
			case err := <-done:
				t.Fatalf("release wrapper returned before history client: %v", err)
			default:
			}
			if action == "Close" {
				select {
				case err := <-closed:
					t.Fatalf("Close returned before release wrapper joined: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			unblock()
			require.Error(t, workerTestReceive(t, done))
			if action == "Close" {
				require.NoError(t, workerTestReceive(t, closed))
			}
		})
	}
}

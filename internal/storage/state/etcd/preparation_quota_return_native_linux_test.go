//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"testing"
	"time"

	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These negative cases use actual TLS etcd. Test-issued publication signatures
// and absent indexes never establish physical death or safe provider cleanup.
func TestColdRuntimeQuotaReturnNative(t *testing.T) {
	for _, mode := range []string{"unpublished", "unsettled", "missing-release", "foreign-workspace", "foreign-task", "foreign-restore"} {
		t.Run(mode, func(t *testing.T) {
			f := newEnrolledNativePublicationFixture(t, false, true)
			ctx, b, c := context.Background(), f.b, f.claim
			// This dependency is never called in these missing-proof negatives.
			b.localRuntimeRelease = &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: time.Second, Provider: &k.TaskRuntimeCleanupClient{}}
			original, err := b.LoadColdPreparation(ctx, c.workspace, c.reference.IntentID)
			require.NoError(t, err)
			if mode != "unpublished" {
				published, err := b.PublishRuntimeEnrolled(ctx, c, f.proof, f.enrollment)
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, published.Outcome)
			}
			if mode != "unpublished" && mode != "unsettled" {
				settled, err := b.SettleColdPreparation(ctx, c.workspace, c.reference.IntentID)
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, settled.Outcome)
			}
			w := c.workspace
			task := TaskReference{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, Partition: w.Partition(), TaskID: uuid.NewString(), SandboxID: c.reference.SandboxID}
			switch mode {
			case "foreign-workspace":
				w, err = NewWorkspaceIdentity("s3", f.options.Identity.StorageID, "bucket", "other/")
				require.NoError(t, err)
			case "foreign-task":
				task.SandboxID = uuid.NewString()
			case "foreign-restore":
				task.RestoreEpoch = "other"
			}
			result, err := b.SettleColdRuntimeQuota(ctx, w, c.reference.IntentID, task)
			require.Error(t, err)
			require.NotEqual(t, OutcomeCommitted, result.Outcome)
			require.Nil(t, result.Entry)
			coldNativeQuotaHeld(t, b, f.raw, *original)
			key, err := b.namespace.coldRuntimeQuotaReturnKey(c.workspace.Partition(), c.reference.IntentID)
			require.NoError(t, err)
			values, err := f.raw.Get(ctx, key)
			require.NoError(t, err)
			require.Empty(t, values.Kvs)
		})
	}
}

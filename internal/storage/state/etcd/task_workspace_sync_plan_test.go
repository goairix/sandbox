package etcd

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func workspaceSyncIdentity(t *testing.T, provider, identity, bucket, prefix string) WorkspaceIdentity {
	t.Helper()
	w, err := NewWorkspaceIdentity(provider, identity, bucket, prefix)
	require.NoError(t, err)
	return w
}

func TestTaskWorkspaceSyncPlanLiteralExclusions(t *testing.T) {
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "storage", "bucket", "workspace/"), Exclude: []string{".agent", "literal*", "a/b"}})
	require.NoError(t, err)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".agent", true}, {".agent/", true}, {".agent/cache/item", true}, {".agentx", false}, {".agentx/cache", false},
		{"literal*", true}, {"literal*/child", true}, {"literal-file", false}, {"a/b", true}, {"a/b/child/", true}, {"a/bc", false}, {"a", false}, {"其他/文件", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got, err := plan.Excludes(tc.path)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	for _, invalid := range []string{"", "/", ".", "..", "/absolute", "a//b", "a/../b", "a/./b", "a\\b", "a//", "./", "a\x00b", "a\nb", "a\u0085b", "a\xffb", strings.Repeat("x", 4097), strings.Repeat("a/", 128) + "a"} {
		_, err := plan.Excludes(invalid)
		require.ErrorIs(t, err, ErrInvalidRecord, "candidate %q", invalid)
	}
}

func TestTaskWorkspaceSyncPlanRejectsInvalidAndBoundedInput(t *testing.T) {
	w := workspaceSyncIdentity(t, "s3", "storage", "bucket", "workspace/")
	for _, invalid := range []string{"", "/", ".", "..", "/absolute", "a//b", "a/../b", "a/./b", "a\\b", "a/", "a\x00b", "a\nb", "a\u0085b", "a\xffb", strings.Repeat("x", 4097), strings.Repeat("a/", 128) + "a"} {
		_, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: []string{invalid}})
		require.ErrorIs(t, err, ErrInvalidRecord, "exclusion %q", invalid)
	}
	entries := make([]string, 256)
	for i := range entries {
		entries[i] = "duplicate"
	}
	_, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: entries})
	require.NoError(t, err)
	_, err = NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: append(entries, "duplicate")})
	require.ErrorIs(t, err, ErrInvalidRecord, "count bound precedes deduplication")
	maxPath := strings.Repeat("x", 4096)
	_, err = NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: []string{maxPath, maxPath, maxPath, maxPath}})
	require.NoError(t, err)
	_, err = NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: []string{maxPath, maxPath, maxPath, maxPath, "x"}})
	require.ErrorIs(t, err, ErrInvalidRecord, "aggregate bound precedes deduplication")
	_, err = NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: []string{strings.Repeat("a/", 127) + "a"}})
	require.NoError(t, err)
	for _, provider := range []string{"local", "s3", "cos", "oss", "obs", "minio"} {
		_, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, provider, "storage", "bucket", "workspace/")})
		require.NoError(t, err)
	}
	for _, workspace := range []WorkspaceIdentity{{}, workspaceSyncIdentity(t, "unknown", "storage", "bucket", "workspace/")} {
		_, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace})
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
}

func TestTaskWorkspaceSyncPlanDigestBindsTargetAndPolicy(t *testing.T) {
	base := TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "storage", "bucket", "workspace/")}
	original, err := NewTaskWorkspaceSyncPlan(base)
	require.NoError(t, err)
	metadata, err := original.FinalSyncPlan()
	require.NoError(t, err)
	require.Equal(t, "sync", metadata.Mode)
	require.NoError(t, metadata.Validate())
	for _, w := range []WorkspaceIdentity{
		workspaceSyncIdentity(t, "minio", "storage", "bucket", "workspace/"),
		workspaceSyncIdentity(t, "s3", "other-storage", "bucket", "workspace/"),
		workspaceSyncIdentity(t, "s3", "storage", "other-bucket", "workspace/"),
		workspaceSyncIdentity(t, "s3", "storage", "bucket", "other-workspace/"),
	} {
		changed := base
		changed.Workspace = w
		plan, err := NewTaskWorkspaceSyncPlan(changed)
		require.NoError(t, err)
		got, err := plan.FinalSyncPlan()
		require.NoError(t, err)
		require.NotEqual(t, metadata.Digest, got.Digest)
	}
	for _, changed := range []TaskWorkspaceSyncPlanOptions{
		{Workspace: base.Workspace, DeleteExtraneous: true}, {Workspace: base.Workspace, Exclude: []string{".agent"}},
	} {
		plan, err := NewTaskWorkspaceSyncPlan(changed)
		require.NoError(t, err)
		got, err := plan.FinalSyncPlan()
		require.NoError(t, err)
		require.NotEqual(t, metadata.Digest, got.Digest)
	}
	empty, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: base.Workspace, Exclude: []string{}})
	require.NoError(t, err)
	got, err := empty.FinalSyncPlan()
	require.NoError(t, err)
	require.Equal(t, metadata, got)
	// These framed components collide under naive concatenation.
	a, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "storage", "ab", "c/")})
	require.NoError(t, err)
	b, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "storage", "a", "bc/")})
	require.NoError(t, err)
	require.NotEqual(t, a.digest, b.digest)
}

func workspaceSyncAttempt(t *testing.T, plan TaskWorkspaceSyncPlan) TaskFinalSyncAttemptRecord {
	t.Helper()
	record := finalSyncRecordFixture(t)
	record.Task.WorkspaceHash = plan.workspace.Hash()
	record.Task.Reference.Partition = plan.workspace.Partition()
	record.Claim.Task = record.Task.Reference
	record.Quiescence.Task = record.Task.Reference
	record.Quiescence.Stage.Partition = plan.workspace.Partition()
	var err error
	record.Plan, err = plan.FinalSyncPlan()
	require.NoError(t, err)
	require.NoError(t, record.Validate())
	return record
}

func TestTaskWorkspaceSyncPlanMatchesOnlyCompleteAttribution(t *testing.T) {
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "storage", "bucket", "workspace/")})
	require.NoError(t, err)
	record := workspaceSyncAttempt(t, plan)
	require.NoError(t, plan.MatchesAttempt(record))
	for _, fault := range []string{"zero", "cross-task", "mode", "opaque-digest", "workspace"} {
		t.Run(fault, func(t *testing.T) {
			changed := record
			want := ErrIdentityMismatch
			switch fault {
			case "zero":
				changed = TaskFinalSyncAttemptRecord{}
				want = ErrInvalidRecord
			case "cross-task":
				changed.Claim.Task.TaskID = "bad"
				want = ErrInvalidRecord
			case "mode":
				changed.Plan.Mode = "none"
			case "opaque-digest":
				changed.Plan.Digest = strings.Repeat("d", 64)
			case "workspace":
				other, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "other-storage", "bucket", "workspace/")})
				require.NoError(t, err)
				changed = workspaceSyncAttempt(t, other)
				changed.Plan = record.Plan
			}
			require.ErrorIs(t, plan.MatchesAttempt(changed), want)
		})
	}
	for _, fault := range []string{"zero", "digest", "policy", "workspace", "unsorted", "redundant"} {
		broken := plan
		switch fault {
		case "zero":
			broken = TaskWorkspaceSyncPlan{}
		case "digest":
			broken.digest = strings.Repeat("d", 64)
		case "policy":
			broken.deleteExtraneous = true
		case "workspace":
			broken.workspace.hash = strings.Repeat("d", 64)
		case "unsorted":
			broken.exclude = []string{"b", "a"}
			broken.digest = broken.computeDigest()
		case "redundant":
			broken.exclude = []string{"a", "a/b"}
			broken.digest = broken.computeDigest()
		}
		_, err := broken.FinalSyncPlan()
		require.ErrorIs(t, err, ErrInvalidRecord, fault)
		require.ErrorIs(t, broken.MatchesAttempt(record), ErrInvalidRecord, fault)
		_, err = broken.Excludes("a")
		require.ErrorIs(t, err, ErrInvalidRecord, fault)
		_, err = (*Backend)(nil).ReserveTaskWorkspaceSync(context.Background(), nil, nil, nil, broken)
		require.ErrorIs(t, err, ErrInvalidRecord, fault)
	}
}

func TestTaskWorkspaceSyncPlanImmutableConcurrentCopies(t *testing.T) {
	input := []string{"a", ".agent"}
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "storage", "bucket", "workspace/"), Exclude: input})
	require.NoError(t, err)
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			copy := plan
			for range 100 {
				got, err := copy.Excludes(".agent/cache")
				if err != nil || !got {
					t.Errorf("copy changed: excluded=%t err=%v", got, err)
				}
			}
		})
	}
	for range 100 {
		input[0], input[1] = input[1], input[0]
	}
	readers.Wait()
}

func TestTaskWorkspaceSyncPlanCanonicalPolicy(t *testing.T) {
	w := workspaceSyncIdentity(t, "s3", "storage", "bucket", "workspace/")
	input := []string{".agent/cache", "literal*", ".agent", ".agent"}
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: input, DeleteExtraneous: true})
	require.NoError(t, err)
	metadata, err := plan.FinalSyncPlan()
	require.NoError(t, err)
	input[0], input[1] = "changed", "also-changed"
	after, err := plan.FinalSyncPlan()
	require.NoError(t, err)
	require.Equal(t, metadata, after, "caller mutation must not alter the original plan")
	equivalent, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: []string{"literal*", ".agent"}, DeleteExtraneous: true})
	require.NoError(t, err)
	canonical, err := equivalent.FinalSyncPlan()
	require.NoError(t, err)
	require.Equal(t, metadata, canonical, "covered descendants and duplicates must not change policy digest")
}

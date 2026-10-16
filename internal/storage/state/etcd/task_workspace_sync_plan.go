package etcd

import (
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/storage"
)

const (
	workspaceSyncMaxExclusions = 256
	workspaceSyncMaxPathBytes  = 4096
	workspaceSyncMaxTotalBytes = 16384
	workspaceSyncMaxDepth      = 128
)

// TaskWorkspaceSyncPlanOptions describes literal exclusion and deletion policy.
type TaskWorkspaceSyncPlanOptions struct {
	Workspace        WorkspaceIdentity
	Exclude          []string
	DeleteExtraneous bool
}

// TaskWorkspaceSyncPlan is immutable attribution data, never a sender capability.
type TaskWorkspaceSyncPlan struct {
	workspace        WorkspaceIdentity
	exclude          []string
	deleteExtraneous bool
	digest           string
}

// NewTaskWorkspaceSyncPlan validates all resource bounds before copying input.
func NewTaskWorkspaceSyncPlan(in TaskWorkspaceSyncPlanOptions) (TaskWorkspaceSyncPlan, error) {
	if validateWorkspaceSyncIdentity(in.Workspace) != nil || validateWorkspaceSyncExclusions(in.Exclude) != nil {
		return TaskWorkspaceSyncPlan{}, ErrInvalidRecord
	}
	exclude := append([]string(nil), in.Exclude...)
	sort.Strings(exclude)
	canonical := exclude[:0]
	for _, candidate := range exclude {
		covered := false
		for _, parent := range canonical {
			if workspaceSyncExcluded(parent, candidate) {
				covered = true
				break
			}
		}
		if !covered {
			canonical = append(canonical, candidate)
		}
	}
	// Do not retain redundant strings in the backing array.
	clear(exclude[len(canonical):])
	p := TaskWorkspaceSyncPlan{workspace: in.Workspace, exclude: canonical, deleteExtraneous: in.DeleteExtraneous}
	p.digest = p.computeDigest()
	return p, nil
}

func validateWorkspaceSyncIdentity(w WorkspaceIdentity) error {
	if w.Kind() != WorkspaceIdentityWorkspace || w.validate() != nil {
		return ErrInvalidRecord
	}
	switch storage.StorageProvider(w.provider) {
	case storage.ProviderLocal, storage.ProviderS3, storage.ProviderCOS, storage.ProviderOSS, storage.ProviderOBS, storage.ProviderMinIO:
		return nil
	default:
		return ErrInvalidRecord
	}
}

func validateWorkspaceSyncExclusions(exclude []string) error {
	if len(exclude) > workspaceSyncMaxExclusions {
		return ErrInvalidRecord
	}
	total := 0
	for _, candidate := range exclude {
		if len(candidate) > workspaceSyncMaxTotalBytes-total || !validWorkspaceSyncPath(candidate) {
			return ErrInvalidRecord
		}
		total += len(candidate)
	}
	return nil
}

func validWorkspaceSyncPath(candidate string) bool {
	if candidate == "" || len(candidate) > workspaceSyncMaxPathBytes || !utf8.ValidString(candidate) || strings.Contains(candidate, "\\") {
		return false
	}
	for _, r := range candidate {
		if unicode.IsControl(r) {
			return false
		}
	}
	depth := 0
	for component := range strings.SplitSeq(candidate, "/") {
		depth++
		if component == "" || component == "." || component == ".." || depth > workspaceSyncMaxDepth {
			return false
		}
	}
	return true
}

func workspaceSyncExcluded(parent, candidate string) bool {
	return candidate == parent || strings.HasPrefix(candidate, parent+"/")
}

func (p TaskWorkspaceSyncPlan) computeDigest() string {
	w := p.workspace
	parts := []string{"sandbox.task-workspace-sync-plan.v1", w.provider, w.storageIdentityHash, w.bucket, w.prefix, w.hash, strconv.FormatBool(p.deleteExtraneous), strconv.Itoa(len(p.exclude))}
	parts = append(parts, p.exclude...)
	digest := framedDigest(parts...)
	return hex.EncodeToString(digest[:])
}

func (p TaskWorkspaceSyncPlan) validate() error {
	if validateWorkspaceSyncIdentity(p.workspace) != nil || validateWorkspaceSyncExclusions(p.exclude) != nil || !validHexDigest(p.digest) {
		return ErrInvalidRecord
	}
	for i, candidate := range p.exclude {
		if i > 0 && p.exclude[i-1] >= candidate {
			return ErrInvalidRecord
		}
		for _, parent := range p.exclude[:i] {
			if workspaceSyncExcluded(parent, candidate) {
				return ErrInvalidRecord
			}
		}
	}
	if p.digest != p.computeDigest() {
		return ErrInvalidRecord
	}
	return nil
}

func (p TaskWorkspaceSyncPlan) FinalSyncPlan() (TaskFinalSyncPlan, error) {
	if p.validate() != nil {
		return TaskFinalSyncPlan{}, ErrInvalidRecord
	}
	return TaskFinalSyncPlan{Mode: "sync", Digest: p.digest}, nil
}

// MatchesAttempt checks structural attribution, not current native authority.
func (p TaskWorkspaceSyncPlan) MatchesAttempt(record TaskFinalSyncAttemptRecord) error {
	if p.validate() != nil || record.Validate() != nil {
		return ErrInvalidRecord
	}
	if record.Task.WorkspaceHash != p.workspace.Hash() || record.Plan.Mode != "sync" || record.Plan.Digest != p.digest {
		return ErrIdentityMismatch
	}
	return nil
}

// Excludes accepts canonical relative candidates and one directory suffix slash.
func (p TaskWorkspaceSyncPlan) Excludes(candidate string) (bool, error) {
	if p.validate() != nil || len(candidate) > workspaceSyncMaxPathBytes {
		return false, ErrInvalidRecord
	}
	candidate = strings.TrimSuffix(candidate, "/")
	if !validWorkspaceSyncPath(candidate) {
		return false, ErrInvalidRecord
	}
	for _, parent := range p.exclude {
		if workspaceSyncExcluded(parent, candidate) {
			return true, nil
		}
	}
	return false, nil
}

package etcd

import (
	"context"
	"reflect"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/kubernetes"
)

type LocalRuntimeReleaseOptions struct {
	MaxActive uint32
	Timeout   time.Duration
	Provider  *kubernetes.TaskRuntimeCleanupClient
}

// TaskRuntimeTerminationIssuer must select the original signed source's exact
// issuer: archive for local, historical quiescence for detached termination.
// Construction invokes neither method; the dependency is borrowed and must
// remain immutable, concurrency safe, and honor cancellation.
type TaskRuntimeTerminationIssuer interface {
	Certificate(context.Context) ([]byte, error)
	SignRuntimeTermination(context.Context, string, controlprotocol.TaskRuntimeTerminationTicketClaims) ([]byte, error)
}

type TaskRuntimeTerminationResult struct {
	Outcome Outcome
	Entry   *TaskRuntimeTerminationEntry
}

type TaskRuntimeTerminationEntry struct {
	Task           TaskReference
	CreateRevision int64
	ModRevision    int64
	SHA256         string
	Phase          string
}

type TaskOwnerReleaseResult struct {
	Outcome Outcome
	Entry   *TaskOwnerReleaseEntry
}

type TaskOwnerReleaseEntry struct {
	Task           TaskReference
	CreateRevision int64
	SHA256         string
}

func (o *LocalRuntimeReleaseOptions) validate() error {
	if o == nil || o.MaxActive < 1 || o.MaxActive > 32 || o.Timeout <= 0 || o.Timeout > 30*time.Second || o.Provider == nil {
		return ErrInvalidConfiguration
	}
	return nil
}

func validateLocalRuntimeRelease(o Options) error {
	if o.TaskRuntimeTerminationIssuer != nil {
		v := reflect.ValueOf(o.TaskRuntimeTerminationIssuer)
		switch v.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if v.IsNil() {
				return ErrInvalidConfiguration
			}
		}
	}
	if o.LocalRuntimeRelease == nil {
		if o.TaskRuntimeTerminationIssuer != nil {
			return ErrInvalidConfiguration
		}
		return nil
	}
	if o.LocalRuntimeRelease.validate() != nil || o.TaskRuntimeTerminationIssuer == nil || o.Clock == nil || o.PublicationTrust == nil || o.EnrolledPublication == nil || o.TaskWorkspaceArchiveRecovery == nil || o.TaskWorkspaceSyncRecyclingBudget == nil {
		return ErrInvalidConfiguration
	}
	if _, err := enrolledPublicationAuthority(o); err != nil {
		return err
	}
	return nil
}

// Slots exist only for bounded active calls. The original claim retains its
// attempt marker after join so a lost ACK cannot reopen sending authority.
type taskLocalRuntimeReleaseOwner struct {
	backend *Backend
	claim   *TaskClaim
	task    TaskReference
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	slot    int
}

func (b *Backend) beginLocalRuntimeRelease(ctx context.Context, c *TaskClaim) (*taskLocalRuntimeReleaseOwner, error) {
	return b.beginLocalRuntimeReleasePurpose(ctx, c, "terminate")
}

func (b *Backend) beginLocalRuntimeReleasePurpose(ctx context.Context, c *TaskClaim, purpose string) (*taskLocalRuntimeReleaseOwner, error) {
	if b == nil || b.localRuntimeRelease.validate() != nil || b.archiveLife == nil {
		return nil, ErrInvalidConfiguration
	}
	if ctx == nil || !b.validTaskClaim(c) {
		return nil, ErrInvalidRecord
	}
	if purpose != "bind" && purpose != "terminate" && purpose != "release" {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.parentCtx.Err(); err != nil {
		return nil, err
	}
	now := time.Now()
	owned, cancel := context.WithTimeout(ctx, b.localRuntimeRelease.Timeout)
	owner := &taskLocalRuntimeReleaseOwner{backend: b, claim: c, task: c.reference.Task, ctx: owned, cancel: cancel, done: make(chan struct{}), slot: -1}
	b.archiveLife.mu.Lock()
	c.mu.Lock()
	unused := purpose == "bind" || purpose == "terminate" && c.runtimeReleaseAttempt == nil || purpose == "release" && c.ownerReleaseAttempt == nil
	if !b.archiveLife.closed && unused && !c.lost && now.Before(c.deadline) {
		slot := -1
		for i := 0; i < int(b.localRuntimeRelease.MaxActive); i++ {
			active := b.archiveLife.runtimeReleases[i]
			if active != nil && active.task == c.reference.Task {
				slot = -1
				break
			}
			if active == nil && slot == -1 {
				slot = i
			}
		}
		if slot >= 0 {
			owner.slot = slot
			b.archiveLife.runtimeReleases[slot] = owner
			if purpose == "terminate" {
				c.runtimeReleaseAttempt = owner
			} else if purpose == "release" {
				c.ownerReleaseAttempt = owner
			}
		}
	}
	c.mu.Unlock()
	b.archiveLife.mu.Unlock()
	if owner.slot < 0 {
		cancel()
		return nil, ErrConflict
	}
	return owner, nil
}

// Read-only diagnosis has the same fixed admission/Close accounting, but needs
// neither a surviving owner/control/index nor a reconstructed TaskClaim.
func (b *Backend) beginLocalRuntimeRead(ctx context.Context, task TaskReference) (*taskLocalRuntimeReleaseOwner, error) {
	if b == nil || b.localRuntimeRelease.validate() != nil || b.archiveLife == nil {
		return nil, ErrInvalidConfiguration
	}
	if ctx == nil || b.validateTaskReference(task) != nil {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owned, cancel := context.WithTimeout(ctx, b.localRuntimeRelease.Timeout)
	o := &taskLocalRuntimeReleaseOwner{backend: b, task: task, ctx: owned, cancel: cancel, done: make(chan struct{}), slot: -1}
	b.archiveLife.mu.Lock()
	if !b.archiveLife.closed {
		for i := 0; i < int(b.localRuntimeRelease.MaxActive); i++ {
			active := b.archiveLife.runtimeReleases[i]
			if active != nil && active.task == task {
				o.slot = -1
				break
			}
			if active == nil && o.slot == -1 {
				o.slot = i
			}
		}
		if o.slot >= 0 {
			b.archiveLife.runtimeReleases[o.slot] = o
		}
	}
	b.archiveLife.mu.Unlock()
	if o.slot < 0 {
		cancel()
		return nil, ErrConflict
	}
	return o, nil
}

// The foreground owner calls finish only after all original dependent calls
// have returned. Close waits for this signal, never just context cancellation.
func (o *taskLocalRuntimeReleaseOwner) finish() {
	life := o.backend.archiveLife
	life.mu.Lock()
	cancel := o.cancel
	life.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	life.mu.Lock()
	if life.runtimeReleases[o.slot] == o {
		life.runtimeReleases[o.slot] = nil
		// Keep the original Claim attempt marker and owner identity, while
		// dropping the caller graph and executable closure before publishing
		// join. Close has already copied any cancel value it still needs.
		o.ctx, o.cancel = nil, nil
		close(o.done)
	}
	life.mu.Unlock()
}

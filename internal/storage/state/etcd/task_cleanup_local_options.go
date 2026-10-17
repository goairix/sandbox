package etcd

import (
	"crypto/ed25519"
	"net"
	"path/filepath"
	"strings"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
)

// LocalCleanupOptions enables known-Task execution on an original DueWorker.
// Concrete credentials, bridge and worker are borrowed for the Backend lifetime;
// callers close them only after Backend.Close. Nodes and storage values are copied.
type LocalCleanupOptions struct {
	Worker             *launcher.KernelBoundary
	Bridge             *k.EnrolledBridgeClient
	Credential         *p.ManagementTLSCredential
	ObserverCredential *p.NodeObserverTLSCredential

	Nodes   []LocalCleanupNode
	Storage LocalCleanupStorage
	// ClaimTTL is integral seconds, 30..300. JobTimeout bounds one original
	// job; ArchiveTimeout must leave at least ten seconds of claim lifetime.
	ClaimTTL, JobTimeout, ArchiveTimeout time.Duration
	// MaxActive bounds queued, admitting and running jobs together, up to 32.
	MaxActive uint32
	// ColdRuntimeQuotaReturn makes automatic cleanup verify the original cold
	// runtime quota return before settling execution or deleting discovery P.
	// It requires PreparationInventory and AutomaticCleanup at worker startup;
	// false preserves the existing cleanup protocol for older deployments.
	ColdRuntimeQuotaReturn bool

	roots   []ed25519.PublicKey
	binding p.TrustBinding
}

// LocalCleanupNode maps an exact enrolled node incarnation to its observer.
// It is fixed deployment configuration, not a per-sandbox runtime registry.
type LocalCleanupNode struct {
	Name, UID, BootID, ObserverAddress string
}

// LocalCleanupStorage identifies one local storage authority and its root.
// PrefixBase restricts the accepted immutable descriptor prefixes beneath root.
type LocalCleanupStorage struct {
	Identity, Bucket, PrefixBase, Directory string
}

// CleanupLocalPhase reports the last confirmed phase, never effect authority.
type CleanupLocalPhase string

// CleanupSlotReference identifies the same permanent occupation across retries.
// Its revisions cannot be supplied to an execution or settlement API.
type CleanupSlotReference struct {
	Task                            TaskReference
	TaskBirth                       int64
	InitialAttempt                  StageAttemptLocator
	Incarnation                     string
	Slot                            uint32
	ExecutionRevision, SlotRevision int64
}

// CleanupPendingReference preserves original uncertain work without authorizing
// a resend. Archive references are diagnostic retained-store coordinates.
type CleanupPendingReference struct {
	Purpose, ClaimID string
	Stage            StageReference
	StoreID          string
	ArchiveSlot      uint32
}

// CleanupLocalResult reports confirmed durable progress. Retained means the
// occupation remains charged; cancellation or an unknown response never frees it.
type CleanupLocalResult struct {
	Task     TaskReference
	Slot     *CleanupSlotReference
	Phase    CleanupLocalPhase
	Outcome  Outcome
	Pending  *CleanupPendingReference
	Release  *TaskOwnerReleaseEntry
	Retained bool
	Reason   string
}

func copyLocalCleanupOptions(o Options) (*LocalCleanupOptions, error) {
	if o.LocalCleanup == nil {
		return nil, nil
	}
	c := *o.LocalCleanup
	if len(c.Nodes) > 256 || len(c.Storage.Identity) > 128 || len(c.Storage.Bucket) > 256 || len(c.Storage.PrefixBase) > 4096 || len(c.Storage.Directory) > 4096 {
		return nil, ErrInvalidConfiguration
	}
	for _, node := range c.Nodes {
		if len(node.Name) > 128 || len(node.UID) > 36 || len(node.BootID) > 36 || len(node.ObserverAddress) > 512 {
			return nil, ErrInvalidConfiguration
		}
	}
	c.Nodes = append([]LocalCleanupNode(nil), c.Nodes...)
	for i, node := range c.Nodes {
		c.Nodes[i] = LocalCleanupNode{strings.Clone(node.Name), strings.Clone(node.UID), strings.Clone(node.BootID), strings.Clone(node.ObserverAddress)}
	}
	c.Storage = LocalCleanupStorage{strings.Clone(c.Storage.Identity), strings.Clone(c.Storage.Bucket), strings.Clone(c.Storage.PrefixBase), strings.Clone(c.Storage.Directory)}
	if c.ClaimTTL == 0 {
		c.ClaimTTL = 120 * time.Second
	}
	if c.JobTimeout == 0 {
		c.JobTimeout = 5 * time.Minute
	}
	if c.ArchiveTimeout == 0 {
		c.ArchiveTimeout = 90 * time.Second
	}
	if c.MaxActive == 0 {
		c.MaxActive = 32
	}
	if o.PublicationTrust != nil {
		if len(o.PublicationTrust.Roots) < 1 || len(o.PublicationTrust.Roots) > 2 {
			return nil, ErrInvalidConfiguration
		}
		c.binding = p.TrustBinding{Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch}
		c.roots = make([]ed25519.PublicKey, len(o.PublicationTrust.Roots))
		for i, root := range o.PublicationTrust.Roots {
			if len(root) != ed25519.PublicKeySize {
				return nil, ErrInvalidConfiguration
			}
			c.roots[i] = append(ed25519.PublicKey(nil), root...)
		}
	}
	return &c, nil
}

func validateLocalCleanupOptions(o Options) error {
	c := o.LocalCleanup
	if c == nil {
		return nil
	}
	if c.ColdRuntimeQuotaReturn && o.PreparationInventory == nil {
		return ErrInvalidConfiguration
	}
	if o.WorkerControl == nil || o.WorkerControl.SessionTTL != 60*time.Second || o.RequestTimeout <= 0 || o.RequestTimeout > 3*time.Second || o.EnrolledPublication == nil || o.PublicationTrust == nil || o.Clock == nil || o.TaskIssuer == nil || o.TaskQuiescenceIssuer == nil || o.TaskWorkspaceArchiveIssuer == nil || o.TaskRuntimeTerminationIssuer == nil || o.LocalRuntimeRelease == nil || o.TaskWorkspaceArchiveRecovery == nil || o.TaskWorkspaceSyncRecyclingBudget == nil {
		return ErrInvalidConfiguration
	}
	if c.Worker == nil || c.Bridge == nil || c.Credential == nil || c.ObserverCredential == nil || c.MaxActive < 1 || c.MaxActive > 32 || len(c.Nodes) < 1 || len(c.Nodes) > 256 || c.ClaimTTL < 30*time.Second || c.ClaimTTL > 300*time.Second || c.ClaimTTL%time.Second != 0 || c.JobTimeout <= 0 || c.JobTimeout > 15*time.Minute || c.ArchiveTimeout <= 0 || c.ArchiveTimeout > 90*time.Second || c.ArchiveTimeout >= c.ClaimTTL-10*time.Second {
		return ErrInvalidConfiguration
	}
	if _, err := c.Worker.WorkspaceSyncWorkerReference(); err != nil {
		return ErrInvalidConfiguration
	}
	if err := c.Worker.ValidateCurrent(); err != nil {
		return ErrInvalidConfiguration
	}
	if c.Storage.Identity == "" || len(c.Storage.Identity) > 128 || c.Storage.Bucket == "" || len(c.Storage.Bucket) > 256 || !validWorkspaceSyncLocalRoot(c.Storage.Directory) || filepath.Clean(c.Storage.Directory) != c.Storage.Directory {
		return ErrInvalidConfiguration
	}
	if c.Storage.PrefixBase != "" && (!strings.HasSuffix(c.Storage.PrefixBase, "/") || !validWorkspaceSyncPath(strings.TrimSuffix(c.Storage.PrefixBase, "/"))) {
		return ErrInvalidConfiguration
	}
	seenNames, seenNodes := map[string]bool{}, map[string]bool{}
	for _, node := range c.Nodes {
		uid, e1 := uuid.Parse(node.UID)
		boot, e2 := uuid.Parse(node.BootID)
		host, port, e3 := net.SplitHostPort(node.ObserverAddress)
		if !validDomainSegment(node.Name) || seenNames[node.Name] || seenNodes[node.UID] || e1 != nil || uid == uuid.Nil || uid.String() != node.UID || e2 != nil || boot == uuid.Nil || boot.String() != node.BootID || e3 != nil || host == "" || port == "" || len(node.ObserverAddress) > 512 {
			return ErrInvalidConfiguration
		}
		seenNames[node.Name], seenNodes[node.UID] = true, true
	}
	return nil
}

package kubernetes

import (
	"context"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"k8s.io/client-go/rest"
)

// TaskRuntimeCleanupStep is a closed set. A step never names an arbitrary API.
type TaskRuntimeCleanupStep string

const (
	TaskRuntimeCleanupInstallRetainFinalizer     TaskRuntimeCleanupStep = "install-retain-finalizer"
	TaskRuntimeCleanupDeletePod                  TaskRuntimeCleanupStep = "delete-pod"
	TaskRuntimeCleanupDeleteNetworkPolicyLogical TaskRuntimeCleanupStep = "delete-network-policy-logical"
	TaskRuntimeCleanupDeleteNetworkPolicyRuntime TaskRuntimeCleanupStep = "delete-network-policy-runtime"
	TaskRuntimeCleanupDeleteCiliumPolicyLogical  TaskRuntimeCleanupStep = "delete-cilium-policy-logical"
	TaskRuntimeCleanupDeleteCiliumPolicyRuntime  TaskRuntimeCleanupStep = "delete-cilium-policy-runtime"
	TaskRuntimeCleanupRemoveRetainFinalizer      TaskRuntimeCleanupStep = "remove-retain-finalizer"
)

type TaskRuntimeCleanupOptions struct {
	Config         *rest.Config
	Namespace      string
	IncludeCilium  bool
	RequestTimeout time.Duration
	MaxActive      uint32
}

type TaskRuntimeCleanupBinding struct {
	Authority        controlprotocol.TrustBinding     `json:"authority"`
	Runtime          controlprotocol.RuntimeReference `json:"runtime"`
	Generation       int64                            `json:"generation"`
	DataGateEpoch    int64                            `json:"data_gate_epoch"`
	TaskID           string                           `json:"task_id"`
	LogicalID        string                           `json:"logical_id"`
	WorkspaceHash    string                           `json:"workspace_hash"`
	CreationIntentID string                           `json:"creation_intent_id"`
	NodeUID          string                           `json:"node_uid"`
	NodeBootID       string                           `json:"node_boot_id"`
	EnrollmentDigest string                           `json:"enrollment_digest"`
	ContractDigest   string                           `json:"contract_digest"`
}

// TaskRuntimeCleanupObservation is copied point-read diagnostic data. It is
// never evidence of process death or authority to release a workspace owner.
type TaskRuntimeCleanupObservation struct {
	Step            TaskRuntimeCleanupStep
	Kind            string
	Name            string
	UID             string
	ResourceVersion string
	Absent          bool
	Deleting        bool
	Retained        bool
	Terminal        bool
}

// The zero value is unusable. Only the concrete constructor installs transport.
type TaskRuntimeCleanupClient struct {
	self   *TaskRuntimeCleanupClient
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	active uint32
	wg     sync.WaitGroup
}

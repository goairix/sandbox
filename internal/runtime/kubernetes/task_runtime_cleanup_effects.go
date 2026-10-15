package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var taskCleanupSteps = [7]TaskRuntimeCleanupStep{
	TaskRuntimeCleanupInstallRetainFinalizer,
	TaskRuntimeCleanupDeletePod,
	TaskRuntimeCleanupDeleteNetworkPolicyLogical,
	TaskRuntimeCleanupDeleteNetworkPolicyRuntime,
	TaskRuntimeCleanupDeleteCiliumPolicyLogical,
	TaskRuntimeCleanupDeleteCiliumPolicyRuntime,
	TaskRuntimeCleanupRemoveRetainFinalizer,
}

func (h *PreparedTaskRuntimeCleanup) enter(ctx context.Context) (context.Context, func(), error) {
	if h == nil || h.self != h {
		return nil, nil, errTaskCleanupRefused
	}
	opCtx, done, err := h.client.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	if h.busy {
		h.mu.Unlock()
		done()
		return nil, nil, errTaskCleanupRefused
	}
	h.busy = true
	h.mu.Unlock()
	return opCtx, func() {
		done()
		h.mu.Lock()
		h.busy = false
		h.mu.Unlock()
	}, nil
}

// ObserveStep reads a fixed slot and retains its exact UID/RV preflight. The
// caller must persist its effect intent and acquire native guards before Apply.
// Refreshing diagnostics never resets a consumed or ambiguous slot.
func (h *PreparedTaskRuntimeCleanup) ObserveStep(ctx context.Context, step TaskRuntimeCleanupStep) (TaskRuntimeCleanupObservation, error) {
	index := slices.Index(taskCleanupSteps[:], step)
	if index < 0 {
		return TaskRuntimeCleanupObservation{}, errTaskCleanupRefused
	}
	opCtx, done, err := h.enter(ctx)
	if err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	defer done()
	observation, err := h.observeSlot(opCtx, index)
	if err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	if err := opCtx.Err(); err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	h.mu.Lock()
	if !h.sent[index] {
		h.preflight[index], h.ready[index] = observation, true
	}
	h.mu.Unlock()
	return observation, nil
}

func (h *PreparedTaskRuntimeCleanup) targetForStep(index int) (taskCleanupTarget, bool) {
	b := h.descriptor.Binding
	if index < 2 || index > 5 {
		return taskCleanupTarget{}, false
	}
	logical := b.LogicalID
	if index == 3 || index == 5 {
		logical = b.Runtime.ID
		if logical == b.LogicalID {
			return taskCleanupTarget{}, false
		}
	}
	kind, prefix := "NetworkPolicy", "sandbox-"
	if index == 4 || index == 5 {
		if !h.client.cilium {
			return taskCleanupTarget{}, false
		}
		kind, prefix = "CiliumNetworkPolicy", "sandbox-private-deny-"
	}
	for _, target := range h.descriptor.Targets {
		if target.Kind == kind && target.Name == prefix+logical {
			return target, true
		}
	}
	return taskCleanupTarget{}, false
}

func (h *PreparedTaskRuntimeCleanup) observeSlot(ctx context.Context, index int) (TaskRuntimeCleanupObservation, error) {
	pod, err := h.pod(ctx)
	if err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	if err := h.node(ctx); err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	observation := h.podObservation(pod)
	observation.Step = taskCleanupSteps[index]
	observation.Terminal = pod == nil
	if index >= 2 && index <= 5 {
		target, enabled := h.targetForStep(index)
		if !enabled {
			// There is no distinct configured resource in a disabled/duplicate
			// slot. This diagnostic is not a receipt that a request was unsent.
			return TaskRuntimeCleanupObservation{Step: taskCleanupSteps[index], Absent: true}, nil
		}
		actual, err := h.policy(ctx, target)
		if err != nil {
			return TaskRuntimeCleanupObservation{}, err
		}
		return TaskRuntimeCleanupObservation{Step: taskCleanupSteps[index], Kind: actual.Kind, Name: actual.Name, UID: actual.UID, ResourceVersion: actual.RV, Absent: actual.UID == "", Terminal: actual.UID == "", Deleting: observation.Deleting, Retained: observation.Retained}, nil
	}
	return observation, nil
}

// Apply consumes one exact preflight and performs at most one mutation. Its
// return value describes that attempted snapshot, not an invented readback or
// physical termination receipt. Even a failed/unknown attempt is not replayed.
func (h *PreparedTaskRuntimeCleanup) Apply(ctx context.Context, step TaskRuntimeCleanupStep) (TaskRuntimeCleanupObservation, error) {
	index := slices.Index(taskCleanupSteps[:], step)
	if index < 0 {
		return TaskRuntimeCleanupObservation{}, errTaskCleanupRefused
	}
	opCtx, done, err := h.enter(ctx)
	if err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	defer done()
	h.mu.Lock()
	if !h.ready[index] || h.sent[index] {
		h.mu.Unlock()
		return TaskRuntimeCleanupObservation{}, errTaskCleanupRefused
	}
	snapshot := h.preflight[index]
	h.ready[index], h.sent[index] = false, true
	h.mu.Unlock()
	current, err := h.observeSlot(opCtx, index)
	if err != nil {
		return snapshot, err
	}
	if snapshot != current {
		return snapshot, errTaskCleanupRefused
	}
	if snapshot.Absent {
		return snapshot, opCtx.Err()
	}
	method, path, contentType := http.MethodDelete, "/api/v1/namespaces/"+h.client.namespace+"/pods/"+h.descriptor.Binding.Runtime.ID, "application/json"
	var body any
	if index == 0 || index == 6 {
		if index == 0 && current.Deleting || index == 6 && !current.Deleting {
			return snapshot, errTaskCleanupRefused
		}
		if index == 0 && current.Retained || index == 6 && !current.Retained {
			return snapshot, nil
		}
		finalizers := []string{}
		if index == 0 {
			finalizers = []string{taskCleanupFinalizer}
		}
		body = []map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": snapshot.UID},
			{"op": "test", "path": "/metadata/resourceVersion", "value": snapshot.ResourceVersion},
			{"op": "add", "path": "/metadata/finalizers", "value": finalizers},
		}
		method, contentType = http.MethodPatch, "application/json-patch+json"
	} else {
		if index == 1 && (!current.Retained || current.Deleting) {
			return snapshot, errTaskCleanupRefused
		}
		if index >= 2 && index <= 5 {
			if !current.Deleting || !current.Retained {
				// A retained deleting Pod is API context only; Backend must
				// separately authenticate physical termination before this call.
				pod, err := h.pod(opCtx)
				if err != nil || pod != nil {
					return snapshot, errorsOrRefused(err)
				}
			}
			target, enabled := h.targetForStep(index)
			if !enabled {
				return snapshot, errTaskCleanupRefused
			}
			path = h.policyPath(target)
		}
		uid, rv := types.UID(snapshot.UID), snapshot.ResourceVersion
		body = metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}
	}
	wire, err := json.Marshal(body)
	if err != nil || len(wire) > taskCleanupBodyLimit {
		return snapshot, errTaskCleanupRefused
	}
	_, err = h.client.api.Verb(method).AbsPath(path).SetHeader("Content-Type", contentType).MaxRetries(0).Body(wire).DoRaw(opCtx)
	return snapshot, err
}

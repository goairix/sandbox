package kubernetes

import (
	"context"
	"fmt"
	"maps"
	"reflect"

	"github.com/goairix/sandbox/internal/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (r *Runtime) ClaimOrdinaryPool(ctx context.Context, ref runtime.RuntimeRef, sandboxID string) error {
	pod, err := r.getExactPod(ctx, ref)
	if err != nil {
		return err
	}
	return r.migrateOrdinaryPoolIdentity(ctx, pod, sandboxID, map[string]*string{"sandbox.pool": nil, "sandbox.id": &sandboxID})
}

func ordinaryPreparedPatchMatches(current, before *corev1.Pod) bool {
	return current != nil && current.UID == before.UID && current.DeletionTimestamp == nil &&
		reflect.DeepEqual(current.Labels, before.Labels) && current.Annotations[ordinaryPreparedStateAnnotation] == "prepared" &&
		current.Annotations[ordinaryPreparedUIDAnnotation] == string(before.UID) &&
		current.Annotations[ordinaryClaimIDAnnotation] == "" && current.Annotations[ordinaryClaimUIDAnnotation] == ""
}

const (
	ordinaryClaimIDAnnotation       = "sandbox.claim.id"
	ordinaryClaimUIDAnnotation      = "sandbox.claim.uid"
	ordinaryPreparedStateAnnotation = "sandbox.pool.state"
	ordinaryPreparedUIDAnnotation   = "sandbox.pool.runtime.uid"
)

// ordinaryPodLabels is the business view, never a patch to the physical CNI
// identity. UID-bound annotations prevent copied metadata authorizing a new Pod.
func ordinaryPodLabels(pod *corev1.Pod) (map[string]string, error) {
	labels := maps.Clone(pod.Labels)
	if pod.Labels["sandbox.workspace.mode"] == "fuse" {
		return labels, nil
	}
	claimed := pod.Annotations[ordinaryClaimIDAnnotation]
	claimUID := pod.Annotations[ordinaryClaimUIDAnnotation]
	if claimed != "" || claimUID != "" {
		if claimed == "" || len(validation.IsDNS1123Subdomain(claimed)) != 0 || claimUID == "" || claimUID != string(pod.UID) || pod.Labels["sandbox.pool"] != "true" {
			return nil, fmt.Errorf("ordinary claim annotation does not match exact Pod identity")
		}
		labels["sandbox.id"] = claimed
		for _, key := range []string{"sandbox.pool", "sandbox.pool.state", "sandbox.pool.key", "sandbox.pool.instance"} {
			delete(labels, key)
		}
		return labels, nil
	}
	state := pod.Annotations[ordinaryPreparedStateAnnotation]
	preparedUID := pod.Annotations[ordinaryPreparedUIDAnnotation]
	if state != "" || preparedUID != "" {
		if state != "prepared" || preparedUID == "" || preparedUID != string(pod.UID) || pod.Labels["sandbox.pool"] != "true" {
			return nil, fmt.Errorf("ordinary prepared annotation does not match exact Pod identity")
		}
		labels["sandbox.pool.state"] = state
	}
	return labels, nil
}

func ordinaryBusinessFilter(key string) bool {
	switch key {
	case "sandbox.id", "sandbox.pool", "sandbox.pool.state", "sandbox.pool.key", "sandbox.pool.instance":
		return true
	default:
		return false
	}
}

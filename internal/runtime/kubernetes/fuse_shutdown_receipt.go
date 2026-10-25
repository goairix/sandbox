package kubernetes

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/goairix/sandbox/internal/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const fuseShutdownReceiptAnnotation = "goairix.github.io/fuse-shutdown-receipt"
const fuseShutdownReceiptDomain = "sandbox/fuse/shutdown-receipt/v1"

// A shutdown acknowledgement is only held in the API process until this
// receipt is committed. Bind it to the exact Pod and preparation attempt so a
// replacement Pod or a copied annotation cannot authorize owner release.
func shutdownReceiptMessage(pod *corev1.Pod, generation int64) []byte {
	return []byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		fuseShutdownReceiptDomain, pod.Namespace, pod.UID, pod.Spec.NodeName,
		pod.Annotations[fusePrepareAttemptAnnotation], generation))
}

func (r *Runtime) validShutdownReceipt(pod *corev1.Pod) bool {
	if pod == nil || len(r.workspaceControlKey) != ed25519.PrivateKeySize {
		return false
	}
	generationRaw, signatureRaw, found := strings.Cut(pod.Annotations[fuseShutdownReceiptAnnotation], ":")
	if !found || signatureRaw == "" {
		return false
	}
	generation, err := strconv.ParseInt(generationRaw, 10, 64)
	if err != nil || generation < 0 || strconv.FormatInt(generation, 10) != generationRaw {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureRaw)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(r.workspaceControlKey.Public().(ed25519.PublicKey),
		shutdownReceiptMessage(pod, generation), signature)
}

// persistShutdownReceipt publishes verified mounter output before Pod deletion.
// An uncertain Update is resolved by reading this exact Pod; shutdown is never
// inferred from a terminal status, and no DELETE follows an unconfirmed write.
func (r *Runtime) persistShutdownReceipt(ctx context.Context, ref runtime.RuntimeRef,
	before *corev1.Pod, generation int64,
) error {
	if before == nil || before.DeletionTimestamp != nil ||
		len(r.workspaceControlKey) != ed25519.PrivateKeySize || !containsString(before.Finalizers, fuseRuntimeCleanupFinalizer) {
		return runtime.ErrTerminationUnconfirmed
	}
	signature := ed25519.Sign(r.workspaceControlKey, shutdownReceiptMessage(before, generation))
	receipt := strconv.FormatInt(generation, 10) + ":" + base64.RawURLEncoding.EncodeToString(signature)

	current, err := r.getExactPod(ctx, ref)
	if err != nil {
		return err
	}
	if current.DeletionTimestamp != nil || current.Spec.NodeName != before.Spec.NodeName ||
		current.Annotations[fusePrepareAttemptAnnotation] != before.Annotations[fusePrepareAttemptAnnotation] ||
		!containsString(current.Finalizers, fuseRuntimeCleanupFinalizer) ||
		validateExactFUSEPodIdentity(current, ref) != nil {
		return runtime.ErrTerminationUnconfirmed
	}
	if existing := current.Annotations[fuseShutdownReceiptAnnotation]; existing != "" {
		if existing == receipt {
			return nil
		}
		return runtime.ErrTerminationUnconfirmed
	}
	updated := current.DeepCopy()
	updated.Annotations[fuseShutdownReceiptAnnotation] = receipt
	_, updateErr := r.client.CoreV1().Pods(r.namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	confirmed, readErr := r.getExactPod(ctx, ref)
	if readErr != nil || confirmed.Spec.NodeName != before.Spec.NodeName ||
		confirmed.Annotations[fusePrepareAttemptAnnotation] != before.Annotations[fusePrepareAttemptAnnotation] ||
		!containsString(confirmed.Finalizers, fuseRuntimeCleanupFinalizer) ||
		confirmed.Annotations[fuseShutdownReceiptAnnotation] != receipt ||
		!r.validShutdownReceipt(confirmed) {
		if updateErr != nil {
			return fmt.Errorf("persist workspace shutdown receipt: %w", updateErr)
		}
		return runtime.ErrTerminationUnconfirmed
	}
	return nil
}

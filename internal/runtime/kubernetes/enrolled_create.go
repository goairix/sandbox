package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
)

var ErrEnrolledCreate = errors.New("enrolled create refused or unresolved")

type EnrolledCreateState string

const (
	EnrolledCreateNotSent  EnrolledCreateState = "not-sent"
	EnrolledCreateUnknown  EnrolledCreateState = "unknown"
	EnrolledCreateObserved EnrolledCreateState = "observed"
)

// EnrolledCreateResult is diagnostic data, never refund or native authority.
type EnrolledCreateResult struct {
	State  EnrolledCreateState
	PodUID string
}

type EnrolledCreateOptions struct {
	API                                  EnrolledBridgeOptions
	Image, NodeName, NodeUID, NodeBootID string
}

// EnrolledCreateClient owns bounded physical API operations. The provisioning
// service must first persist the unique dispatch attempt; this driver does not
// confer that authority or remember per-sandbox attempts in process memory.
type EnrolledCreateClient struct {
	self                                 *EnrolledCreateClient
	api                                  *EnrolledBridgeClient
	image, nodeName, nodeUID, nodeBootID string
}

func NewEnrolledCreateClient(o EnrolledCreateOptions) (*EnrolledCreateClient, error) {
	if _, err := kubecontract.EnrolledPlainContractDigest(o.Image); err != nil || len(o.Image) > 1024 || len(o.NodeName) > 128 || len(validation.IsDNS1123Subdomain(o.NodeName)) != 0 || !coldCreateUUID(o.NodeUID) || !coldCreateUUID(o.NodeBootID) {
		return nil, ErrEnrolledCreate
	}
	api, err := NewEnrolledBridgeClient(o.API)
	if err != nil {
		return nil, errors.Join(ErrEnrolledCreate, err)
	}
	c := &EnrolledCreateClient{api: api, image: strings.Clone(o.Image), nodeName: strings.Clone(o.NodeName), nodeUID: strings.Clone(o.NodeUID), nodeBootID: strings.Clone(o.NodeBootID)}
	c.self = c
	return c, nil
}

func (c *EnrolledCreateClient) Close() error {
	if c == nil || c.self != c {
		return ErrEnrolledCreate
	}
	return c.api.Close()
}

func (c *EnrolledCreateClient) CreateOnce(ctx context.Context, request EnrolledPlainPodRequest) (EnrolledCreateResult, error) {
	return c.exchange(ctx, request, "", true)
}

func (c *EnrolledCreateClient) InspectOriginal(ctx context.Context, request EnrolledPlainPodRequest, originalUID string) (EnrolledCreateResult, error) {
	return c.exchange(ctx, request, originalUID, false)
}

func coldCreateUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (c *EnrolledCreateClient) exchange(ctx context.Context, request EnrolledPlainPodRequest, originalUID string, create bool) (result EnrolledCreateResult, err error) {
	result.State = EnrolledCreateUnknown
	if create {
		result.State = EnrolledCreateNotSent
	}
	if c == nil || c.self != c || ctx == nil || originalUID != "" && !coldCreateUUID(originalUID) || request.Spec.Image != c.image || request.NodeName != c.nodeName || request.NodeUID != c.nodeUID || request.NodeBootID != c.nodeBootID {
		return result, ErrEnrolledCreate
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	pod, err := BuildEnrolledPlainPod(c.api.namespace, request)
	if err != nil {
		return result, errors.Join(ErrEnrolledCreate, err)
	}
	pod.APIVersion, pod.Kind = "v1", "Pod"
	wire, err := json.Marshal(pod)
	if err != nil || len(wire) > taskCleanupBodyLimit {
		return result, ErrEnrolledCreate
	}
	method, path := http.MethodGet, "/api/v1/namespaces/"+c.api.namespace+"/pods/"+pod.Name
	var body io.Reader
	if create {
		method, path, body = http.MethodPost, "/api/v1/namespaces/"+c.api.namespace+"/pods", bytes.NewReader(wire)
	}
	// Strip caller context values, including HTTP trace callbacks. Each accepted
	// operation owns its socket and is joined by the private client's Close.
	bounded, cancel := context.WithTimeout(context.Background(), c.api.timeout)
	op := &bridgeOperation{client: c.api, cancel: cancel}
	c.api.mu.Lock()
	if c.api.closed || len(c.api.active) >= c.api.limit {
		c.api.mu.Unlock()
		cancel()
		return result, ErrEnrolledCreate
	}
	c.api.active[op] = struct{}{}
	c.api.wg.Add(1)
	c.api.mu.Unlock()
	stopCaller, stopBounded := joinedAfterFunc(ctx, op.abort), joinedAfterFunc(bounded, op.abort)
	defer func() { stopCaller(); stopBounded(); op.finish() }()
	u := c.api.config.origin
	u.Path, u.RawPath = path, ""
	req, err := http.NewRequestWithContext(bounded, method, u.String(), body)
	if err != nil {
		return result, err
	}
	req.Close = true
	req.Header.Set("Accept", "application/json")
	if create {
		req.Header.Set("Content-Type", "application/json")
	}
	c.api.config.authorize(req)
	conn, err := c.api.dialAPI(bounded, op)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	// Mark unknown before the first possible byte. Any partial write, response
	// loss, rejection, or malformed reply retains that outcome; never retry POST.
	result.State = EnrolledCreateUnknown
	if err := req.Write(conn); err != nil {
		return result, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return result, err
	}
	defer func() { conn.Close(); response.Body.Close() }()
	expectedStatus := http.StatusOK
	if create {
		expectedStatus = http.StatusCreated
	}
	if response.StatusCode != expectedStatus || len(response.Header.Values("Content-Encoding")) != 0 || len(response.Header.Values("Upgrade")) != 0 || response.ContentLength > taskCleanupBodyLimit {
		return result, ErrEnrolledCreate
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, taskCleanupBodyLimit+1))
	if err != nil {
		return result, err
	}
	var observed corev1.Pod
	if len(data) > taskCleanupBodyLimit || json.Unmarshal(data, &observed) != nil || !coldCreateMatches(pod, &observed, originalUID) {
		return result, ErrEnrolledCreate
	}
	return EnrolledCreateResult{State: EnrolledCreateObserved, PodUID: string(observed.UID)}, nil
}

func coldCreateMatches(expected, observed *corev1.Pod, originalUID string) bool {
	if observed.APIVersion != "v1" || observed.Kind != "Pod" || !coldCreateUUID(string(observed.UID)) || originalUID != "" && string(observed.UID) != originalUID || observed.Name != expected.Name || observed.Namespace != expected.Namespace || observed.GenerateName != "" || observed.DeletionTimestamp != nil || len(observed.OwnerReferences) != 0 || !equality.Semantic.DeepEqual(observed.Labels, expected.Labels) || !equality.Semantic.DeepEqual(observed.Annotations, expected.Annotations) || !equality.Semantic.DeepEqual(observed.Finalizers, expected.Finalizers) {
		return false
	}
	if err := kubecontract.ValidateEnrolledPlainPod(observed); err != nil {
		return false
	}
	actual := observed.Spec.DeepCopy()
	// Match the explicit benign priority defaults already accepted by the
	// enrolled contract; all remaining security/configuration fields are exact.
	if actual.PriorityClassName == "" && actual.Priority != nil && *actual.Priority == 0 {
		actual.Priority = nil
	}
	if actual.PriorityClassName == "" && actual.PreemptionPolicy != nil && *actual.PreemptionPolicy == corev1.PreemptLowerPriority {
		actual.PreemptionPolicy = nil
	}
	return equality.Semantic.DeepEqual(*actual, expected.Spec)
}

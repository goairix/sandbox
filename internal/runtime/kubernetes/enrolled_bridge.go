package kubernetes

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
)

var ErrEnrolledBridge = errors.New("enrolled bridge refused")

type EnrolledBridgeOptions struct {
	Config         *rest.Config
	Namespace      string
	MaxActive      int
	RequestTimeout time.Duration
}

// EnrolledBridgeTarget is a copied candidate, never native authority evidence.
type EnrolledBridgeTarget struct {
	PodName, PodUID, NodeName, NodeUID, NodeBootID string
	LogicalID, CreationIntentID                    string
	Generation                                     int64
	Authority                                      p.TrustBinding
	ContractDigest                                 string
	UID, GID                                       uint32
	NetworkAllowed                                 bool
}

type EnrolledBridgeClient struct {
	self      *EnrolledBridgeClient
	config    *staticAPIConfig
	namespace string
	limit     int
	timeout   time.Duration
	mu        sync.Mutex
	closed    bool
	active    map[*bridgeOperation]struct{}
	wg        sync.WaitGroup
}

func NewEnrolledBridgeClient(o EnrolledBridgeOptions) (*EnrolledBridgeClient, error) {
	if o.MaxActive < 1 || o.MaxActive > 16 || o.RequestTimeout <= 0 || o.RequestTimeout > 5*time.Second || len(validation.IsDNS1123Label(o.Namespace)) != 0 {
		return nil, ErrEnrolledBridge
	}
	snapshot, err := snapshotStaticAPI(o.Config)
	if err != nil {
		return nil, errors.Join(ErrEnrolledBridge, err)
	}
	c := &EnrolledBridgeClient{config: snapshot, namespace: strings.Clone(o.Namespace), limit: o.MaxActive, timeout: o.RequestTimeout, active: make(map[*bridgeOperation]struct{})}
	c.self = c
	return c, nil
}

type bridgeOperation struct {
	client *EnrolledBridgeClient
	mu     sync.Mutex
	raw    net.Conn
	closed bool
	conn   net.Conn
	cancel context.CancelFunc
	once   sync.Once
}

func (o *bridgeOperation) own(raw net.Conn) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		raw.Close()
		return net.ErrClosed
	}
	o.raw = raw
	return nil
}

// abort closes the physical transport before any protocol locks or joins.
func (o *bridgeOperation) abort() {
	o.mu.Lock()
	o.closed = true
	raw := o.raw
	o.mu.Unlock()
	o.cancel()
	if raw != nil {
		raw.Close()
	}
}

func (o *bridgeOperation) finish() {
	o.once.Do(func() {
		o.abort()
		o.client.mu.Lock()
		delete(o.client.active, o)
		o.client.mu.Unlock()
		o.client.wg.Done()
	})
}

// joinedAfterFunc stops admission to a callback and waits if it already began.
func joinedAfterFunc(ctx context.Context, f func()) func() {
	done := make(chan struct{})
	var once sync.Once
	stop := context.AfterFunc(ctx, func() { defer close(done); f() })
	return func() {
		once.Do(func() {
			if !stop() {
				<-done
			}
		})
	}
}

func (c *EnrolledBridgeClient) Close() error {
	if c == nil || c.self != c {
		return ErrEnrolledBridge
	}
	c.mu.Lock()
	c.closed = true
	ops := make([]*bridgeOperation, 0, len(c.active))
	for op := range c.active {
		ops = append(ops, op)
	}
	c.mu.Unlock()
	for _, op := range ops {
		op.abort()
	}
	for _, op := range ops {
		op.mu.Lock()
		conn := op.conn
		op.mu.Unlock()
		if conn != nil {
			conn.Close()
		}
	}
	c.wg.Wait()
	return nil
}

func (c *EnrolledBridgeClient) Dial(ctx context.Context, target EnrolledBridgeTarget) (net.Conn, error) {
	if c == nil || c.self != c || ctx == nil || !validBridgeTarget(target) {
		return nil, ErrEnrolledBridge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Do not pass caller context values (including HTTP trace callbacks) to I/O.
	bounded, cancel := context.WithTimeout(context.Background(), c.timeout)
	op := &bridgeOperation{client: c, cancel: cancel}
	c.mu.Lock()
	if c.closed || len(c.active) >= c.limit {
		c.mu.Unlock()
		cancel()
		return nil, ErrEnrolledBridge
	}
	c.active[op] = struct{}{}
	c.wg.Add(1)
	c.mu.Unlock()
	stopCaller := joinedAfterFunc(ctx, op.abort)
	stopBounded := joinedAfterFunc(bounded, op.abort)
	transferred := false
	defer func() {
		stopCaller()
		stopBounded()
		cancel()
		if !transferred {
			op.finish()
		}
	}()
	if err := c.checkTarget(bounded, op, target); err != nil {
		return nil, err
	}
	conn, err := c.upgrade(bounded, op, target)
	if err != nil {
		return nil, err
	}
	stopCaller()
	stopBounded()
	op.mu.Lock()
	if op.closed || ctx.Err() != nil || bounded.Err() != nil {
		op.mu.Unlock()
		conn.Close()
		return nil, ErrEnrolledBridge
	}
	op.conn = conn
	transferred = true
	op.mu.Unlock()
	return conn, nil
}

func validBridgeTarget(t EnrolledBridgeTarget) bool {
	for _, name := range []string{t.PodName, t.NodeName, t.LogicalID} {
		if len(name) > 128 || len(validation.IsDNS1123Subdomain(name)) != 0 {
			return false
		}
	}
	for _, value := range []string{t.PodUID, t.NodeUID, t.NodeBootID, t.CreationIntentID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return false
		}
	}
	return t.Generation > 0 && t.UID > 0 && t.UID <= 2147483647 && t.GID > 0 && t.GID <= 2147483647 && taskCleanupHash(t.ContractDigest) && taskCleanupNamespace(t.Authority.Namespace) && taskCleanupOpaque(t.Authority.AuthorityID) && taskCleanupOpaque(t.Authority.Target) && len(t.Authority.RestoreEpoch) <= 128 && taskCleanupSegment(t.Authority.RestoreEpoch)
}

func (c *EnrolledBridgeClient) checkTarget(ctx context.Context, op *bridgeOperation, target EnrolledBridgeTarget) error {
	var pod v1.Pod
	if err := c.get(ctx, op, "/api/v1/namespaces/"+c.namespace+"/pods/"+target.PodName, &pod); err != nil {
		return err
	}
	profile, err := kubecontract.InspectEnrolledPlainPod(&pod)
	if err != nil || pod.APIVersion != "v1" || pod.Kind != "Pod" || pod.Namespace != c.namespace || pod.Name != target.PodName || string(pod.UID) != target.PodUID || pod.DeletionTimestamp != nil || pod.Spec.NodeName != target.NodeName || profile.LogicalID != target.LogicalID || profile.CreationIntentID != target.CreationIntentID || profile.Generation != target.Generation || profile.NodeUID != target.NodeUID || profile.NodeBootID != target.NodeBootID || profile.Config.TrustBinding() != target.Authority || profile.Config.ContractDigest != target.ContractDigest || profile.Config.UID != target.UID || profile.Config.GID != target.GID || profile.Config.NetworkAllowed != target.NetworkAllowed {
		return ErrEnrolledBridge
	}
	var node v1.Node
	if err := c.get(ctx, op, "/api/v1/nodes/"+target.NodeName, &node); err != nil {
		return err
	}
	if node.APIVersion != "v1" || node.Kind != "Node" || node.Namespace != "" || node.Name != target.NodeName || string(node.UID) != target.NodeUID || node.DeletionTimestamp != nil || node.Status.NodeInfo.BootID != target.NodeBootID || node.Status.NodeInfo.OperatingSystem != "linux" {
		return ErrEnrolledBridge
	}
	return nil
}

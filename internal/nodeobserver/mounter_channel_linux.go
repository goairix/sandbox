//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/kubecontract"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This is a read-only, kernel-authenticated side channel of an existing slot,
// not a second enrollment registry. Only its original handler owns the helper
// anchor and IO; cancellation closes the connection, then retirement closes
// the anchor before the same slot may accept a replacement channel.
type mounterHealthChannel struct {
	conn     *mounterConnection
	anchor   *processAnchor
	launcher *processAnchor

	identity p.RuntimeIdentityContext
	poolKey  string

	requests chan mounterPull
	done     chan struct{}
}

// mounterConnection owns the one original Unix close. Stop and pull timeout
// may close it before the handler retires; sync.Once preserves that first
// result instead of hiding a real failure behind a later net.ErrClosed.
// reported is used only by the original registration handler's nested defers.
type mounterConnection struct {
	*net.UnixConn
	closeOnce sync.Once
	closeErr  error
	reported  bool
}

func (c *mounterConnection) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.UnixConn.Close()
		if errors.Is(c.closeErr, net.ErrClosed) {
			c.closeErr = nil
		}
	})
	return c.closeErr
}

type mounterPull struct {
	ctx    context.Context
	anchor time.Time
	reply  chan mounterPullResult
}

type mounterPullResult struct {
	sample   fuseprotocol.MounterReadiness
	deadline time.Time
	err      error
}

type mounterRegistration struct {
	Version        int    `json:"version"`
	RuntimeUID     string `json:"runtime_uid"`
	PodName        string `json:"pod_name"`
	NodeUID        string `json:"node_uid"`
	NodeBootID     string `json:"node_boot_id"`
	ContractDigest string `json:"contract_digest"`
}

// mounterSampleWindow owns monotonic deadlines; the two source UTC times are
// diagnostic only. A repeated sequence can shorten but never refresh its local
// deadline, including across a reconnect of the same original helper anchor.
type mounterSampleWindow struct {
	sample   fuseprotocol.MounterReadiness
	deadline time.Time
}

func (w *mounterSampleWindow) accept(sample fuseprotocol.MounterReadiness, anchor, received time.Time) (fuseprotocol.MounterReadiness, error) {
	wire, err := json.Marshal(sample)
	if err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	if _, err := fuseprotocol.DecodeMounterReadiness(wire); err != nil || anchor.IsZero() || received.Before(anchor) {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter sample is invalid")
	}
	deadline := anchor.Add(time.Duration(sample.ValidForNanoseconds))
	if w.sample.SampleSequence != 0 {
		old := w.sample
		if sample.RuntimeUID != old.RuntimeUID || sample.PoolKey != old.PoolKey || sample.Generation != old.Generation || sample.MountAttempt != old.MountAttempt || sample.MountID != old.MountID || sample.SampleSequence < old.SampleSequence {
			return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter sample identity/sequence changed")
		}
		if sample.SampleSequence == old.SampleSequence {
			if !sample.SampledAt.Equal(old.SampledAt) || !sample.CacheObservedAt.Equal(old.CacheObservedAt) {
				return fuseprotocol.MounterReadiness{}, fmt.Errorf("unchanged mounter sequence replaced source times")
			}
			if w.deadline.Before(deadline) {
				deadline = w.deadline
			}
		}
	}
	// Remember even an expired, otherwise well-formed sequence. A reconnect
	// must not reset the window and turn that same late sample positive.
	w.sample, w.deadline = sample, deadline
	if !received.Before(deadline) {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter sample expired in transit")
	}
	sample.ValidForNanoseconds = int64(deadline.Sub(received))
	return sample, nil
}

func mounterContainerIDMatches(containerID, group string) bool {
	if len(group) > 4096 || !strings.HasPrefix(group, "/") || path.Clean(group) != group || strings.ContainsAny(group, "\x00\r\n\\") {
		return false
	}
	scheme, id, ok := strings.Cut(containerID, "://")
	decoded, err := hex.DecodeString(id)
	if !ok || err != nil || len(decoded) != 32 || strings.ToLower(id) != id {
		return false
	}
	prefix := ""
	switch scheme {
	case "containerd":
		prefix = "cri-containerd-"
	case "cri-o":
		prefix = "crio-"
	case "docker":
		prefix = "docker-"
	default:
		return false
	}
	component := path.Base(group)
	return component == id || component == prefix+id+".scope"
}

// mounterProfile makes bounded registration-only API observations. The first
// read runs the unchanged provider guards; the second obtains a complete FUSE
// snapshot and exact running init ContainerID, independently rechecking its
// original tuple. Neither result is treated as a new activation authority.
// Stable pulls below perform no Pod GET, exec or management operation.
func (o *Observer) mounterProfile(ctx context.Context, registration providerRegistration) (string, string, error) {
	if _, err := o.provider.observe(ctx, registration, false); err != nil {
		return "", "", err
	}
	pod, err := o.provider.client.CoreV1().Pods(o.provider.namespace).Get(ctx, registration.Identity.Runtime.ID, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	profile, err := kubecontract.InspectEnrolledFUSEPod(pod)
	if err != nil || string(pod.UID) != registration.Identity.Runtime.UID || pod.Spec.NodeName != o.provider.nodeName || len(pod.ResourceVersion) == 0 || len(pod.ResourceVersion) > 128 || pod.DeletionTimestamp != nil || profile.LogicalID != registration.Identity.SandboxID || profile.Generation != registration.Identity.Generation || profile.NodeUID != o.provider.nodeUID || profile.NodeBootID != o.provider.nodeBootID || profile.CreationIntentID != registration.CreationIntentID || profile.Config.TrustBinding() != registration.Binding || profile.Config.ContractDigest != registration.Birth.ContractDigest || profile.Config.UID != registration.Birth.UID || profile.Config.GID != registration.Birth.GID || profile.Config.NetworkAllowed != registration.Birth.NetworkAllowed {
		return "", "", fmt.Errorf("mounter profile differs from original enrollment")
	}
	if len(pod.Status.InitContainerStatuses) != 1 {
		return "", "", fmt.Errorf("running mounter ContainerID unavailable")
	}
	status := pod.Status.InitContainerStatuses[0]
	if status.Name != "workspace-mounter" || status.RestartCount != 0 || status.State.Running == nil || status.ContainerID == "" {
		return "", "", fmt.Errorf("mounter init container is not the original running instance")
	}
	return status.ContainerID, profile.Bootstrap.PoolKey, nil
}

func validateMounterProcess(anchor, launcher *processAnchor) error {
	if anchor == nil || launcher == nil || anchor.facts.PIDNS == launcher.facts.PIDNS || anchor.facts.MountNS == launcher.facts.MountNS {
		return fmt.Errorf("mounter must have independent private PID/mount namespaces")
	}
	var own unix.Stat_t
	if unix.Stat("/proc/self/ns/mnt", &own) != nil || (descriptorIdentity{Device: uint64(own.Dev), Inode: own.Ino}) == anchor.facts.MountNS {
		return fmt.Errorf("mounter shares observer mount namespace")
	}
	status, err := readProcAt(anchor.proc, "status", 65536)
	if err != nil || privatePID1(status) != nil {
		return fmt.Errorf("mounter lacks original private PID1")
	}
	seen := make(map[string]bool, 2)
	for _, line := range strings.Split(string(status), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 0 || parts[0] != "Uid:" && parts[0] != "Gid:" {
			continue
		}
		if seen[parts[0]] || len(parts) != 5 {
			return fmt.Errorf("ambiguous mounter root credentials")
		}
		seen[parts[0]] = true
		for _, value := range parts[1:] {
			if value != "0" {
				return fmt.Errorf("mounter is not the original root helper")
			}
		}
	}
	if !seen["Uid:"] || !seen["Gid:"] {
		return fmt.Errorf("mounter root credentials unavailable")
	}
	// The privileged helper is a trusted system component, not confined by
	// AppArmor. Its deployment policy name is already bound by the immutable
	// FUSE contract; applying launcher's limited capability gate here would be
	// a false kernel assertion. No helper anchor reaches termination signaling.
	return anchor.revalidate()
}

func (o *Observer) handleMounterHealth(ctx context.Context, conn *net.UnixConn) error {
	return o.serveMounterHealth(ctx, &mounterConnection{UnixConn: conn}, nil)
}

func (o *Observer) serveMounterHealth(ctx context.Context, conn *mounterConnection, registered func()) error {
	pid, err := kernelPeer(conn.UnixConn)
	if err != nil {
		return err
	}
	auth, cancel := context.WithTimeout(ctx, o.options.RequestTimeout)
	defer cancel()
	deadline, _ := auth.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	wire, err := t.ReadNodeObserverFrame(conn)
	if err != nil {
		return err
	}
	var request fuseprotocol.SocketRequest
	var hello mounterRegistration
	if fuseprotocol.DecodeExact(wire, &request) != nil || request.Version != 1 || request.Command != "register-readiness" || fuseprotocol.DecodeExact(request.Input, &hello) != nil || hello.Version != 2 || hello.NodeUID != o.options.NodeUID || hello.NodeBootID != o.identity.NodeBootID() {
		return fmt.Errorf("invalid mounter registration locator")
	}

	// A public hello cannot reserve a slot. Locate only an original verified
	// enrollment whose launcher has actually returned with its kernel anchor.
	o.mu.Lock()
	slot := -1
	var activation p.TargetActivationEvidence
	var launcher *processAnchor
	var registration providerRegistration
	for i := range o.slots {
		entry := &o.slots[i]
		if o.stopping || entry.registration.Version != 1 || entry.anchor == nil || entry.running || entry.mounter != nil || o.journal == nil || o.journal.records[i].State != "enrolled" {
			continue
		}
		candidate, err := o.verifier.VerifyNodeEnrollmentRequest(entry.registration.Request, entry.registration.ObservedAt)
		if err != nil {
			continue
		}
		identity := candidate.Identity()
		if identity.Runtime.UID == hello.RuntimeUID && identity.Runtime.ID == hello.PodName && candidate.Birth().ContractDigest == hello.ContractDigest {
			if slot >= 0 {
				o.mu.Unlock()
				return fmt.Errorf("ambiguous original mounter enrollment")
			}
			slot, activation, launcher = i, candidate, entry.anchor
			registration = providerRegistration{Binding: candidate.Binding(), Identity: identity, Birth: candidate.Birth(), CreationIntentID: entry.registration.Provider.CreationIntentID}
		}
	}
	o.mu.Unlock()
	if slot < 0 {
		return fmt.Errorf("mounter original enrollment unavailable")
	}
	containerID, poolKey, err := o.mounterProfile(auth, registration)
	if err != nil {
		return err
	}
	anchor, err := openProcess(pid)
	if err != nil {
		return err
	}
	owner := &mounterHealthChannel{conn: conn, anchor: anchor, launcher: launcher, identity: activation.Identity(), poolKey: poolKey, requests: make(chan mounterPull), done: make(chan struct{})}
	defer func() {
		closeErr := errors.Join(conn.Close(), anchor.close())
		conn.reported = true
		o.finishMounterRetirement(slot, owner, closeErr)
	}()
	if !mounterContainerIDMatches(containerID, anchor.facts.Cgroup.Path) {
		return fmt.Errorf("mounter cgroup differs from exact init ContainerID")
	}
	if err := validateMounterProcess(anchor, launcher); err != nil {
		return err
	}
	if err := o.revalidateMounterLauncher(slot, launcher, nil); err != nil {
		return err
	}
	if boot, err := bootID(); err != nil || boot != hello.NodeBootID {
		return fmt.Errorf("mounter node boot changed")
	}
	o.mu.Lock()
	entry := &o.slots[slot]
	if o.stopping || auth.Err() != nil || entry.anchor != launcher || entry.running || entry.mounter != nil || o.journal.records[slot].State != "enrolled" || entry.mounterBound && entry.mounterKernel != anchor.facts {
		o.mu.Unlock()
		return fmt.Errorf("original mounter slot changed during registration")
	}
	entry.mounterBound, entry.mounterKernel = true, anchor.facts
	entry.mounter = owner
	o.mu.Unlock()
	if err := writeMounterResponse(conn, []byte(`{"version":2,"accepted":true}`)); err != nil {
		return err
	}
	if registered != nil {
		registered()
	}
	cancel()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pull := <-owner.requests:
			sample, err := o.pullMounter(slot, owner, pull)
			o.mu.Lock()
			deadline := o.slots[slot].mounterWindow.deadline
			o.mu.Unlock()
			pull.reply <- mounterPullResult{sample: sample, deadline: deadline, err: err}
			if err != nil {
				return err
			}
		}
	}
}

func writeMounterResponse(conn *mounterConnection, output []byte) error {
	wire, err := json.Marshal(fuseprotocol.SocketResponse{Version: fuseprotocol.Version, OK: true, Output: output})
	if err != nil {
		return err
	}
	return t.WriteNodeObserverFrame(conn, wire)
}

// revalidateMounterLauncher serializes every read of the launcher's owned FDs
// with executeTermination's terminal close under this same mutex. Checking a
// pointer, unlocking, then revalidating is insufficient: that path can close
// the same anchor in between. Only synchronous kernel reads occur under mu;
// the caller performs all network IO outside this critical section.
func (o *Observer) revalidateMounterLauncher(slot int, launcher *processAnchor, owner *mounterHealthChannel) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if slot < 0 || slot >= len(o.slots) || launcher == nil || o.stopping || o.journal == nil {
		return fmt.Errorf("mounter original launcher unavailable")
	}
	entry := &o.slots[slot]
	if entry.anchor != launcher || entry.running || entry.mounter != owner || o.journal.records[slot].State != "enrolled" {
		return fmt.Errorf("mounter original launcher changed")
	}
	return launcher.revalidate()
}

// finishMounterRetirement runs after the original channel loop has returned
// and both original Close operations completed. A real close failure cannot
// certify retirement: preserve any installed owner and seal further admission.
func (o *Observer) finishMounterRetirement(slot int, owner *mounterHealthChannel, closeErr error) {
	o.mu.Lock()
	if closeErr == nil && o.slots[slot].mounter == owner {
		o.slots[slot].mounter = nil
	}
	o.closeErr = errors.Join(o.closeErr, closeErr)
	close(owner.done)
	o.mu.Unlock()
	if closeErr != nil {
		o.stop()
	}
}

// An unauthenticated or over-budget connection has no reusable slot to retain.
// Its original close still must be certified; call outside mu because sealing
// admission closes the remaining original connections under that mutex.
func (o *Observer) closeUnregisteredMounter(conn *mounterConnection) {
	if err := conn.Close(); err != nil {
		o.mu.Lock()
		o.closeErr = errors.Join(o.closeErr, err)
		o.mu.Unlock()
		o.stop()
	}
}

// ReadMounterReadiness performs one finite diagnostic pull through an existing
// verified slot. Caller fields only select that slot and must match its exact
// authenticated identity and pool. The result confers no execution, mount,
// flush, termination or release authority. Missing, busy or stale is Unknown.
func (o *Observer) ReadMounterReadiness(ctx context.Context, identity p.RuntimeIdentityContext, poolKey string, generation int64, mountAttempt uint8) (fuseprotocol.MounterReadiness, error) {
	if o == nil || ctx == nil || ctx.Err() != nil || generation != identity.Generation || mountAttempt != 1 {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("invalid mounter readiness locator/context")
	}
	o.mu.Lock()
	var owner *mounterHealthChannel
	slot := -1
	for i := range o.slots {
		entry := &o.slots[i]
		if !o.stopping && !entry.running && entry.anchor != nil && entry.mounter != nil && o.journal != nil && o.journal.records[i].State == "enrolled" && entry.mounter.identity == identity && entry.mounter.poolKey == poolKey {
			owner = entry.mounter
			slot = i
			break
		}
	}
	if owner == nil {
		o.mu.Unlock()
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("authenticated mounter channel unavailable")
	}
	select {
	case o.rpcs <- struct{}{}:
	default:
		o.mu.Unlock()
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter pull budget busy")
	}
	o.mu.Unlock()
	defer func() { <-o.rpcs }()
	bounded, cancel := context.WithTimeout(ctx, o.options.RequestTimeout)
	defer cancel()
	pull := mounterPull{ctx: bounded, anchor: time.Now(), reply: make(chan mounterPullResult, 1)}
	select {
	case owner.requests <- pull:
	case <-owner.done:
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter channel retired")
	case <-bounded.Done():
		return fuseprotocol.MounterReadiness{}, bounded.Err()
	}
	select {
	case result := <-pull.reply:
		if bounded.Err() != nil {
			return fuseprotocol.MounterReadiness{}, bounded.Err()
		}
		if result.err != nil {
			return fuseprotocol.MounterReadiness{}, result.err
		}
		// Scheduling after the handler replied is still elapsed time. It must
		// neither refresh the duration nor expose a result after Close/Destroy.
		o.mu.Lock()
		entry := &o.slots[slot]
		remaining := result.deadline.Sub(time.Now())
		valid := !o.stopping && !entry.running && entry.mounter == owner && entry.anchor == owner.launcher && o.journal.records[slot].State == "enrolled" && remaining > 0
		o.mu.Unlock()
		if !valid {
			return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter sample expired or original owner retired")
		}
		result.sample.ValidForNanoseconds = min(result.sample.ValidForNanoseconds, int64(remaining))
		return result.sample, nil
	case <-owner.done:
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter channel retired")
	case <-bounded.Done():
		_ = owner.conn.Close()
		return fuseprotocol.MounterReadiness{}, bounded.Err()
	}
}

func (o *Observer) pullMounter(slot int, owner *mounterHealthChannel, pull mounterPull) (fuseprotocol.MounterReadiness, error) {
	if err := pull.ctx.Err(); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	if err := owner.anchor.revalidate(); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	if err := o.revalidateMounterLauncher(slot, owner.launcher, owner); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	deadline, _ := pull.ctx.Deadline()
	if err := owner.conn.SetDeadline(deadline); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	wire, err := json.Marshal(fuseprotocol.SocketRequest{Version: 1, Command: "health-ready", Input: json.RawMessage(`{"version":2}`)})
	if err == nil {
		err = t.WriteNodeObserverFrame(owner.conn, wire)
	}
	if err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	wire, err = t.ReadNodeObserverFrame(owner.conn)
	if err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	var response fuseprotocol.SocketResponse
	if fuseprotocol.DecodeExact(wire, &response) != nil || response.Version != 1 || !response.OK || response.ErrorCode != "" {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter sample is Unknown")
	}
	sample, err := fuseprotocol.DecodeMounterReadiness(response.Output)
	if err != nil || sample.RuntimeUID != owner.identity.Runtime.UID || sample.Generation != owner.identity.Generation || sample.PoolKey != owner.poolKey || sample.MountAttempt != 1 {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter sample differs from authenticated tuple")
	}
	if err := owner.anchor.revalidate(); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	if err := o.revalidateMounterLauncher(slot, owner.launcher, owner); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entry := &o.slots[slot]
	if o.stopping || entry.mounter != owner || entry.anchor != owner.launcher || entry.running || o.journal.records[slot].State != "enrolled" || pull.ctx.Err() != nil {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("mounter original owner changed during pull")
	}
	return entry.mounterWindow.accept(sample, pull.anchor, time.Now())
}

func (o *Observer) acceptMounter(listener *net.UnixListener) error {
	for {
		raw, err := listener.AcceptUnix()
		if err != nil {
			if o.ctx.Err() != nil {
				return nil
			}
			return err
		}
		conn := &mounterConnection{UnixConn: raw}
		o.mu.Lock()
		select {
		case o.rpcs <- struct{}{}:
			if o.stopping {
				<-o.rpcs
				o.mu.Unlock()
				o.closeUnregisteredMounter(conn)
				continue
			}
			o.connections[conn] = struct{}{}
			o.ioWG.Add(1)
		default:
			o.mu.Unlock()
			o.closeUnregisteredMounter(conn)
			continue
		}
		o.mu.Unlock()
		go func() {
			defer o.ioWG.Done()
			var released sync.Once
			release := func() { released.Do(func() { <-o.rpcs }) }
			defer release()
			defer func() {
				if !conn.reported {
					o.closeUnregisteredMounter(conn)
				}
				o.mu.Lock()
				delete(o.connections, conn)
				o.mu.Unlock()
			}()
			_ = o.serveMounterHealth(o.ctx, conn, release)
		}()
	}
}

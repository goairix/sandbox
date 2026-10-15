package nodeobserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const nativeWireLimit = 256 * 1024

type authorityEnrollment struct {
	Identity                                                      p.RuntimeIdentityContext
	CreationIntentID, EnrollmentDigest, ObserverCertificateDigest string
	NodeUID, NodeBootID                                           string
}

type authorityReader struct {
	client    *clientv3.Client
	verifier  *p.ManagementVerifier
	clock     p.AuthorityClock
	binding   p.TrustBinding
	identity  []byte
	clusterID uint64
	timeout   time.Duration
}

type authoritySnapshot struct {
	reader            *authorityReader
	keys              p.TaskRuntimeTerminationKeys
	record            p.TaskRuntimeTerminationIntent
	ticket            p.TaskRuntimeTerminationEvidence
	identity, restore *mvccpb.KeyValue
	domain            [8]*mvccpb.KeyValue
	history           [6]*mvccpb.KeyValue
	complete, intent  *mvccpb.KeyValue
	claim, guard      *mvccpb.KeyValue
	cutoff            time.Time
}

func digest(wire []byte) string {
	sum := sha256.Sum256(wire)
	return hex.EncodeToString(sum[:])
}

func (s *authoritySnapshot) live(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(s.cutoff) {
		return fmt.Errorf("observer native authority cutoff expired")
	}
	return nil
}

func exactComparisons(kv *mvccpb.KeyValue) []clientv3.Cmp {
	k := string(kv.Key)
	return []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(k), "=", kv.CreateRevision), clientv3.Compare(clientv3.ModRevision(k), "=", kv.ModRevision), clientv3.Compare(clientv3.Value(k), "=", string(kv.Value)), clientv3.Compare(clientv3.LeaseValue(k), "=", kv.Lease)}
}

func (s *authoritySnapshot) comparisons() []clientv3.Cmp {
	cmps := []clientv3.Cmp{
		clientv3.Compare(clientv3.Value(s.keys.Identity), "=", string(s.identity.Value)), clientv3.Compare(clientv3.LeaseValue(s.keys.Identity), "=", int64(0)),
		clientv3.Compare(clientv3.Value(s.keys.Restore), "=", string(s.restore.Value)), clientv3.Compare(clientv3.LeaseValue(s.keys.Restore), "=", int64(0)),
	}
	for _, kv := range s.domain {
		cmps = append(cmps, exactComparisons(kv)...)
	}
	for _, kv := range []*mvccpb.KeyValue{s.claim, s.guard} {
		cmps = append(cmps, exactComparisons(kv)...)
	}
	for _, kv := range s.history {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(string(kv.Key)), "=", kv.ModRevision))
	}
	cmps = append(cmps, exactComparisons(s.complete)...)
	cmps = append(cmps, exactComparisons(s.intent)...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(s.keys.OperationsPrefix), "=", int64(0)).WithRange(clientv3.GetPrefixRangeEnd(s.keys.OperationsPrefix)))
	return cmps
}

func protoPoint(key string) *pb.RequestOp {
	return &pb.RequestOp{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(key)}}}
}

func pointReplyBudget(keys []string, limits []int) *pb.TxnResponse {
	h := &pb.ResponseHeader{ClusterId: math.MaxUint64, MemberId: math.MaxUint64, Revision: math.MaxInt64, RaftTerm: math.MaxUint64}
	r := &pb.TxnResponse{Header: h, Succeeded: true}
	for i, key := range keys {
		kv := &mvccpb.KeyValue{Key: []byte(key), Value: make([]byte, limits[i]), CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64, Version: math.MaxInt64, Lease: math.MaxInt64}
		r.Responses = append(r.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Header: h, Kvs: []*mvccpb.KeyValue{kv}, Count: 1}}})
	}
	return r
}

func (s *authoritySnapshot) guardRequest() (*pb.TxnRequest, error) {
	cmps := s.comparisons()
	request := &pb.TxnRequest{Success: []*pb.RequestOp{protoPoint(s.keys.Identity)}}
	for _, cmp := range cmps {
		wire := pb.Compare(cmp)
		request.Compare = append(request.Compare, &wire)
	}
	if len(request.Compare) != 59 || len(request.Success)+len(request.Compare) != 60 || request.Size() > nativeWireLimit || pointReplyBudget([]string{s.keys.Identity}, []int{4096}).Size() > nativeWireLimit || pointReplyBudget(nil, nil).Size() > nativeWireLimit {
		return nil, fmt.Errorf("observer native guard exceeds fixed 59+1/wire budget")
	}
	return request, nil
}

func headerOK(h *pb.ResponseHeader, cluster uint64) bool {
	return h != nil && h.ClusterId == cluster && h.Revision > 0 && len(h.XXX_unrecognized) == 0
}

func (r *authorityReader) points(ctx context.Context, s *authoritySnapshot, keys []string, limits []int) ([]*mvccpb.KeyValue, error) {
	if len(keys) == 0 || len(keys) > 4 || len(keys) != len(limits) || pointReplyBudget(keys, limits).Size() > nativeWireLimit {
		return nil, fmt.Errorf("observer point batch exceeds bound")
	}
	if err := s.live(ctx); err != nil {
		return nil, err
	}
	request := &pb.TxnRequest{}
	ops := make([]clientv3.Op, len(keys))
	for i, key := range keys {
		if len(key) > 1024 || !bytes.HasPrefix([]byte(key), []byte(r.binding.Namespace)) {
			return nil, fmt.Errorf("observer point key outside binding")
		}
		ops[i] = clientv3.OpGet(key)
		request.Success = append(request.Success, protoPoint(key))
	}
	if request.Size() > nativeWireLimit {
		return nil, fmt.Errorf("observer point request exceeds bound")
	}
	rpc, cancel := context.WithTimeout(ctx, r.timeout)
	response, err := r.client.Txn(rpc).Then(ops...).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil || s.live(ctx) != nil {
		return nil, errors.Join(err, rpcErr, s.live(ctx))
	}
	if response == nil || !headerOK(response.Header, r.clusterID) || !response.Succeeded || len(response.XXX_unrecognized) != 0 || len(response.Responses) != len(keys) {
		return nil, fmt.Errorf("invalid observer point transaction header/shape")
	}
	result := make([]*mvccpb.KeyValue, len(keys))
	for i, key := range keys {
		op := response.Responses[i]
		if op == nil || len(op.XXX_unrecognized) != 0 || op.GetResponseRange() == nil {
			return nil, fmt.Errorf("observer requires flat point replies")
		}
		rangeResponse := op.GetResponseRange()
		if len(rangeResponse.XXX_unrecognized) != 0 || rangeResponse.More || rangeResponse.Count != 1 || len(rangeResponse.Kvs) != 1 {
			return nil, fmt.Errorf("observer fixed point missing or oversized")
		}
		if h := rangeResponse.Header; h != nil && (len(h.XXX_unrecognized) != 0 || h.ClusterId != 0 && h.ClusterId != response.Header.ClusterId || h.Revision != 0 && h.Revision != response.Header.Revision || h.MemberId != 0 && h.MemberId != response.Header.MemberId || h.RaftTerm != 0 && h.RaftTerm != response.Header.RaftTerm) {
			return nil, fmt.Errorf("observer nested point header mismatch")
		}
		kv := rangeResponse.Kvs[0]
		if kv == nil || len(kv.XXX_unrecognized) != 0 || string(kv.Key) != key || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > response.Header.Revision || kv.Version <= 0 || kv.Version > kv.ModRevision-kv.CreateRevision+1 || len(kv.Value) > limits[i] {
			return nil, fmt.Errorf("invalid observer point key/envelope/size")
		}
		result[i] = kv
	}
	if (*pb.TxnResponse)(response).Size() > nativeWireLimit {
		return nil, fmt.Errorf("observer point response exceeds wire bound")
	}
	return result, nil
}

func (r *authorityReader) load(ctx context.Context, wire, issuerWire []byte, enrollment authorityEnrollment, operationCutoff time.Time) (*authoritySnapshot, error) {
	if len(wire) == 0 || len(wire) > 8192 || len(issuerWire) == 0 || len(issuerWire) > 4096 {
		return nil, fmt.Errorf("observer ticket/certificate wire bound")
	}
	// Unauthenticated fields select only a bounded fixed task point. They do
	// not become expected values or grant any effect before persisted decode.
	var incoming struct {
		Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
	}
	if err := json.Unmarshal(wire, &incoming); err != nil {
		return nil, err
	}
	c := incoming.Claims
	if _, err := p.EncodeTaskRuntimeTerminationClaim(c.Current, c.Partition); err != nil || c.Current.Namespace != r.binding.Namespace || c.Current.AuthorityID != r.binding.AuthorityID || c.Current.Target != r.binding.Target || c.Current.RestoreEpoch != r.binding.RestoreEpoch {
		return nil, fmt.Errorf("observer task locator outside fixed binding")
	}
	s := &authoritySnapshot{reader: r, cutoff: operationCutoff}
	root := r.binding.Namespace
	intentKey := fmt.Sprintf("%sp/%02x/tasks/%s/runtime-termination", root, c.Partition, c.Current.TaskID)
	initial, err := r.points(ctx, s, []string{root + "meta/identity", root + "meta/restore_epoch", intentKey}, []int{4096, 128, 16384})
	if err != nil {
		return nil, err
	}
	s.identity, s.restore, s.intent = initial[0], initial[1], initial[2]
	if s.identity.Lease != 0 || s.restore.Lease != 0 || s.intent.Lease != 0 || !bytes.Equal(s.identity.Value, r.identity) || string(s.restore.Value) != r.binding.RestoreEpoch {
		return nil, fmt.Errorf("observer deployment identity/restore mismatch")
	}
	s.record, err = p.DecodeTaskRuntimeTerminationIntent(s.intent.Value)
	if err != nil || s.record.Phase != "reserved" || s.record.Current != c.Current || s.record.Partition != c.Partition || s.record.EnrollmentDigest != enrollment.EnrollmentDigest || s.record.ObserverCertificateDigest != enrollment.ObserverCertificateDigest || s.record.NodeUID != enrollment.NodeUID || s.record.NodeBootID != enrollment.NodeBootID || s.record.CreationIntentID != enrollment.CreationIntentID || (p.RuntimeIdentityContext{SandboxID: c.Current.SandboxID, WorkspaceHash: c.Current.WorkspaceHash, Generation: c.Current.Generation, Runtime: c.Current.Runtime}) != enrollment.Identity {
		return nil, fmt.Errorf("observer reserved intent/enrollment mismatch: %v", err)
	}
	s.keys, err = p.TaskRuntimeTerminationKeysFor(s.record)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	clockCtx, clockCancel := context.WithTimeout(ctx, time.Second)
	observation, err := r.clock.Observe(clockCtx)
	clockErr := clockCtx.Err()
	clockCancel()
	elapsed := time.Since(started)
	if err != nil || clockErr != nil || observation.UTC.IsZero() || observation.UTC.Location() != time.UTC || observation.Uncertainty < 0 || observation.Uncertainty > time.Second || elapsed > time.Second-observation.Uncertainty {
		return nil, fmt.Errorf("observer controlled clock unavailable")
	}
	observation.UTC = observation.UTC.Add(elapsed)
	observation.Uncertainty += elapsed
	expected := p.TaskRuntimeTerminationTicketClaims{Version: 1, Purpose: "task_runtime_terminate", Current: s.record.Current, Partition: s.record.Partition, IntentCreateRevision: s.intent.CreateRevision, IntentModRevision: s.intent.ModRevision, IntentDigest: digest(s.intent.Value), EnrollmentDigest: s.record.EnrollmentDigest, ObserverCertificateDigest: s.record.ObserverCertificateDigest, NodeUID: s.record.NodeUID, NodeBootID: s.record.NodeBootID, NotBefore: c.NotBefore, NotAfter: c.NotAfter}
	s.ticket, err = r.verifier.VerifyTaskRuntimeTerminationTicket(wire, issuerWire, expected, observation.UTC)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(c.NotAfter.Sub(observation.UTC) - observation.Uncertainty)
	if deadline.Before(s.cutoff) {
		s.cutoff = deadline
	}
	a := s.record.Authority
	keys := []string{s.keys.Task, s.keys.CleanupIntent, s.keys.TaskLink, s.keys.Placement, s.keys.Control, s.keys.Owner, s.keys.Fence, s.keys.RuntimeIndex, s.keys.Quiescence, s.keys.QuiescenceReceipt, s.keys.QuiescenceReservation, s.keys.FinalSync, s.keys.Issuer, s.keys.Archive, s.keys.LocalComplete, s.keys.Claim, s.keys.Guard}
	stamps := []p.RecordStamp{a.Task, a.CleanupIntent, a.TaskLink, a.Placement, a.Control, a.Owner, a.Fence, a.RuntimeIndex, a.Quiescence, a.QuiescenceReceipt, a.QuiescenceReservation, a.FinalSync, a.Issuer, a.Archive, a.LocalComplete}
	var records []*mvccpb.KeyValue
	for start := 0; start < len(keys); start += 4 {
		end := min(start+4, len(keys))
		limits := make([]int, end-start)
		for i := range limits {
			limits[i] = 32768
		}
		points, err := r.points(ctx, s, keys[start:end], limits)
		if err != nil {
			return nil, err
		}
		records = append(records, points...)
	}
	for i, stamp := range stamps {
		kv := records[i]
		if kv.Lease != 0 || kv.CreateRevision != stamp.CreateRevision || kv.ModRevision != stamp.ModRevision || digest(kv.Value) != stamp.SHA256 || !json.Valid(kv.Value) {
			return nil, fmt.Errorf("observer native record stamp mismatch at fixed point %d", i)
		}
	}
	copy(s.domain[:], records[:8])
	copy(s.history[:], records[8:14])
	s.complete, s.claim, s.guard = records[14], records[15], records[16]
	claim, err := p.EncodeTaskRuntimeTerminationClaim(c.Current, c.Partition)
	if err != nil {
		return nil, err
	}
	for _, kv := range []*mvccpb.KeyValue{s.claim, s.guard} {
		if kv.Lease != c.Current.LeaseID || kv.Lease <= 0 || kv.CreateRevision != c.Current.ClaimCreateRevision || kv.ModRevision != kv.CreateRevision || kv.Version != 1 || !bytes.Equal(kv.Value, claim) {
			return nil, fmt.Errorf("observer native original claim/guard mismatch")
		}
	}
	// Domain semantic matching is mandatory before a snapshot may be guarded.
	if err := validateAuthorityDomain(s, enrollment, issuerWire); err != nil {
		return nil, err
	}
	if err := s.live(ctx); err != nil {
		return nil, err
	}
	ttlStart := time.Now()
	rpc, cancel := context.WithTimeout(ctx, r.timeout)
	// The high-level clientv3 TTL response discards the native header. Use the
	// concrete client's same pinned TLS connection so cluster validation remains
	// mandatory; this read never grants or renews the Lease.
	ttl, err := pb.NewLeaseClient(r.client.ActiveConnection()).LeaseTimeToLive(rpc, &pb.LeaseTimeToLiveRequest{ID: c.Current.LeaseID})
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil || ttl == nil || !headerOK(ttl.Header, r.clusterID) || ttl.ID != c.Current.LeaseID || ttl.TTL <= 0 || ttl.GrantedTTL < ttl.TTL || ttl.GrantedTTL > 86400 || len(ttl.XXX_unrecognized) != 0 {
		return nil, fmt.Errorf("observer original claim Lease is not live")
	}
	leaseCutoff := ttlStart.Add(time.Duration(ttl.TTL) * time.Second)
	if leaseCutoff.Before(s.cutoff) {
		s.cutoff = leaseCutoff
	}
	if err := s.live(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *authoritySnapshot) revalidate(ctx context.Context) error {
	if err := s.live(ctx); err != nil {
		return err
	}
	if _, err := s.guardRequest(); err != nil {
		return err
	}
	r := s.reader
	rpc, cancel := context.WithTimeout(ctx, r.timeout)
	response, err := r.client.Txn(rpc).If(s.comparisons()...).Then(clientv3.OpGet(s.keys.Identity)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil || s.live(ctx) != nil {
		return errors.Join(err, rpcErr, s.live(ctx))
	}
	if err := s.validateGuardReply((*pb.TxnResponse)(response)); err != nil {
		return err
	}
	return s.live(ctx)
}

func (s *authoritySnapshot) validateGuardReply(response *pb.TxnResponse) error {
	r := s.reader
	if response == nil || !headerOK(response.Header, r.clusterID) || len(response.XXX_unrecognized) != 0 {
		return fmt.Errorf("observer final native guard header invalid")
	}
	if !response.Succeeded {
		if len(response.Responses) != 0 {
			return fmt.Errorf("observer failed guard returned unexpected branch")
		}
		return fmt.Errorf("observer final native authority lost")
	}
	if len(response.Responses) != 1 || response.Responses[0] == nil || len(response.Responses[0].XXX_unrecognized) != 0 {
		return fmt.Errorf("observer final native guard response shape")
	}
	point := response.Responses[0].GetResponseRange()
	if point == nil || len(point.XXX_unrecognized) != 0 || point.More || point.Count != 1 || len(point.Kvs) != 1 || point.Kvs[0] == nil || string(point.Kvs[0].Key) != s.keys.Identity || point.Kvs[0].Lease != 0 || !bytes.Equal(point.Kvs[0].Value, r.identity) {
		return fmt.Errorf("observer final native guard identity read invalid")
	}
	if h := point.Header; h != nil && (len(h.XXX_unrecognized) != 0 || h.ClusterId != 0 && h.ClusterId != response.Header.ClusterId || h.Revision != 0 && h.Revision != response.Header.Revision || h.MemberId != 0 && h.MemberId != response.Header.MemberId || h.RaftTerm != 0 && h.RaftTerm != response.Header.RaftTerm) {
		return fmt.Errorf("observer final native guard nested header mismatch")
	}
	kv := point.Kvs[0]
	if len(kv.XXX_unrecognized) != 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > response.Header.Revision || kv.Version <= 0 || kv.Version > kv.ModRevision-kv.CreateRevision+1 {
		return fmt.Errorf("observer final native guard identity metadata invalid")
	}
	if response.Size() > nativeWireLimit {
		return fmt.Errorf("observer final native guard response exceeds bound")
	}
	return nil
}

// These private wire structs reproduce only the fixed independently checked
// native domain records. They confer no backend capability and import no store.
type authorityTaskReference struct {
	Namespace    string `json:"namespace"`
	RestoreEpoch string `json:"restore_epoch"`
	TaskID       string `json:"task_id"`
	SandboxID    string `json:"sandbox_id"`
	Partition    uint8  `json:"partition"`
}

type authorityTaskRecord struct {
	Version          uint32                 `json:"version"`
	Reference        authorityTaskReference `json:"reference"`
	Kind             string                 `json:"kind"`
	WorkspaceHash    string                 `json:"workspace_hash"`
	CreationIntentID string                 `json:"creation_intent_id"`
	Generation       int64                  `json:"generation"`
	DataGateEpoch    int64                  `json:"data_gate_epoch"`
	ControlRevision  int64                  `json:"control_revision"`
	Runtime          p.RuntimeReference     `json:"runtime"`
	Snapshot         p.SnapshotReference    `json:"snapshot"`
	ExpiresAt        time.Time              `json:"expires_at"`
}

type authorityStage struct {
	Namespace    string `json:"namespace"`
	Partition    uint8  `json:"partition"`
	RequestID    string `json:"request_id"`
	StageID      string `json:"stage_id"`
	AttemptID    string `json:"attempt_id"`
	RestoreEpoch string `json:"restore_epoch"`
}

func canonicalNative(wire []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(value); err != nil || !bytes.Equal(bytes.TrimSuffix(b.Bytes(), []byte("\n")), wire) {
		return fmt.Errorf("noncanonical observer native domain record")
	}
	return nil
}

func validateAuthorityDomain(s *authoritySnapshot, enrollment authorityEnrollment, issuerWire []byte) error {
	c := s.record.Current
	ref := authorityTaskReference{c.Namespace, c.RestoreEpoch, c.TaskID, c.SandboxID, s.record.Partition}
	var task authorityTaskRecord
	if canonicalNative(s.domain[0].Value, &task) != nil || task.Version != 1 || task.Reference != ref || task.Kind != "cleanup" || task.WorkspaceHash != c.WorkspaceHash || task.CreationIntentID != enrollment.CreationIntentID || task.Generation != c.Generation || task.DataGateEpoch != c.DataGateEpoch || task.ControlRevision >= c.ControlRevision || task.ControlRevision <= 0 || task.Runtime != enrollment.Identity.Runtime || task.Snapshot.Version == "" || len(task.Snapshot.Digest) != 64 || task.ExpiresAt.IsZero() || digest(s.domain[0].Value) != c.TaskDigest || s.domain[0].CreateRevision != s.record.TaskBirth || s.domain[0].ModRevision != s.domain[0].CreateRevision {
		return fmt.Errorf("observer native task does not match registered incarnation")
	}
	var cleanup struct {
		Version uint32              `json:"version"`
		Task    authorityTaskRecord `json:"task"`
		Attempt authorityStage      `json:"attempt"`
	}
	if canonicalNative(s.domain[1].Value, &cleanup) != nil || cleanup.Version != 1 || cleanup.Task != task || cleanup.Attempt.Namespace != c.Namespace || cleanup.Attempt.Partition != s.record.Partition || cleanup.Attempt.RestoreEpoch != c.RestoreEpoch || s.domain[1].CreateRevision != s.domain[1].ModRevision {
		return fmt.Errorf("observer native cleanup task mismatch")
	}
	var link struct {
		Version   uint32                 `json:"version"`
		Reference authorityTaskReference `json:"reference"`
	}
	if canonicalNative(s.domain[2].Value, &link) != nil || link.Version != 1 || link.Reference != ref || s.domain[2].CreateRevision != s.domain[2].ModRevision {
		return fmt.Errorf("observer native sandbox task link mismatch")
	}
	var placement struct {
		Version       uint32 `json:"version"`
		SandboxID     string `json:"sandbox_id"`
		WorkspaceHash string `json:"workspace_hash"`
		IntentID      string `json:"intent_id"`
		RestoreEpoch  string `json:"restore_epoch"`
		Partition     uint8  `json:"partition"`
		Generation    int64  `json:"generation"`
	}
	if canonicalNative(s.domain[3].Value, &placement) != nil || placement.Version != 1 || placement.SandboxID != c.SandboxID || placement.WorkspaceHash != c.WorkspaceHash || placement.IntentID != enrollment.CreationIntentID || placement.RestoreEpoch != c.RestoreEpoch || placement.Partition != s.record.Partition || placement.Generation != c.Generation {
		return fmt.Errorf("observer native placement mismatch")
	}
	var control struct {
		Version       uint32              `json:"version"`
		SandboxID     string              `json:"sandbox_id"`
		WorkspaceHash string              `json:"workspace_hash"`
		IntentID      string              `json:"intent_id"`
		RestoreEpoch  string              `json:"restore_epoch"`
		Generation    int64               `json:"generation"`
		DataGateEpoch int64               `json:"data_gate_epoch"`
		Phase         string              `json:"phase"`
		Snapshot      p.SnapshotReference `json:"snapshot"`
		Runtime       *p.RuntimeReference `json:"runtime,omitempty"`
		MountAttempt  uint8               `json:"mount_attempt"`
		ExpiresAt     time.Time           `json:"expires_at"`
	}
	if canonicalNative(s.domain[4].Value, &control) != nil || control.Version != 1 || control.SandboxID != c.SandboxID || control.WorkspaceHash != c.WorkspaceHash || control.IntentID != enrollment.CreationIntentID || control.RestoreEpoch != c.RestoreEpoch || control.Generation != c.Generation || control.DataGateEpoch != c.DataGateEpoch || control.Phase != "destroying" || control.Runtime == nil || *control.Runtime != enrollment.Identity.Runtime || control.MountAttempt != s.record.MountAttempt || s.domain[4].ModRevision != c.ControlRevision || control.Snapshot != task.Snapshot {
		return fmt.Errorf("observer native destroying control mismatch")
	}
	var owner struct {
		Version       uint32              `json:"version"`
		WorkspaceHash string              `json:"workspace_hash"`
		SandboxID     string              `json:"sandbox_id"`
		IntentID      string              `json:"intent_id"`
		RestoreEpoch  string              `json:"restore_epoch"`
		Generation    int64               `json:"generation"`
		Runtime       *p.RuntimeReference `json:"runtime,omitempty"`
		MountAttempt  uint8               `json:"mount_attempt"`
	}
	if canonicalNative(s.domain[5].Value, &owner) != nil || owner.Version != 1 || owner.WorkspaceHash != c.WorkspaceHash || owner.SandboxID != c.SandboxID || owner.IntentID != enrollment.CreationIntentID || owner.RestoreEpoch != c.RestoreEpoch || owner.Generation != c.Generation || owner.Runtime == nil || *owner.Runtime != enrollment.Identity.Runtime || owner.MountAttempt != s.record.MountAttempt {
		return fmt.Errorf("observer native workspace owner mismatch")
	}
	var fence struct {
		Version       uint32 `json:"version"`
		WorkspaceHash string `json:"workspace_hash"`
		RestoreEpoch  string `json:"restore_epoch"`
		Generation    int64  `json:"generation"`
	}
	if canonicalNative(s.domain[6].Value, &fence) != nil || fence.Version != 1 || fence.WorkspaceHash != c.WorkspaceHash || fence.RestoreEpoch != c.RestoreEpoch || fence.Generation != c.Generation {
		return fmt.Errorf("observer native workspace fence mismatch")
	}
	var index struct {
		Version       uint32             `json:"version"`
		IntentID      string             `json:"intent_id"`
		SandboxID     string             `json:"sandbox_id"`
		WorkspaceHash string             `json:"workspace_hash"`
		RestoreEpoch  string             `json:"restore_epoch"`
		Generation    int64              `json:"generation"`
		Runtime       p.RuntimeReference `json:"runtime"`
	}
	if canonicalNative(s.domain[7].Value, &index) != nil || index.Version != 1 || index.IntentID != enrollment.CreationIntentID || index.SandboxID != c.SandboxID || index.WorkspaceHash != c.WorkspaceHash || index.RestoreEpoch != c.RestoreEpoch || index.Generation != c.Generation || index.Runtime != enrollment.Identity.Runtime || s.domain[7].CreateRevision != s.domain[7].ModRevision {
		return fmt.Errorf("observer native runtime index mismatch")
	}
	var issuer struct {
		Version           uint32          `json:"version"`
		Namespace         string          `json:"namespace"`
		RestoreEpoch      string          `json:"restore_epoch"`
		CertificateID     string          `json:"certificate_id"`
		CertificateDigest string          `json:"certificate_digest"`
		Certificate       json.RawMessage `json:"certificate"`
	}
	if canonicalNative(s.history[4].Value, &issuer) != nil || issuer.Version != 1 || issuer.Namespace != c.Namespace || issuer.RestoreEpoch != c.RestoreEpoch || issuer.CertificateID != c.IssuerCertificateID || issuer.CertificateDigest != c.IssuerCertificateDigest || !bytes.Equal(issuer.Certificate, issuerWire) || digest(issuerWire) != c.IssuerCertificateDigest || s.history[4].CreateRevision != s.history[4].ModRevision {
		return fmt.Errorf("observer native current command issuer mismatch")
	}
	return nil
}

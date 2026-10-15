//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func retainedQuery(record journalRecord, enrollment string) (p.NodeTerminationQuery, error) {
	var ticket struct {
		Claims    p.TaskRuntimeTerminationTicketClaims `json:"claims"`
		Signature []byte                               `json:"signature"`
	}
	if canonicalObserver(record.Ticket, &ticket) != nil {
		return p.NodeTerminationQuery{}, fmt.Errorf("retained observer ticket corrupt")
	}
	intent, err := p.DecodeTaskRuntimeTerminationIntent(record.Intent)
	c := ticket.Claims
	if err != nil || intent.Phase != "reserved" || c.Current != intent.Current || c.Partition != intent.Partition || c.EnrollmentDigest != enrollment || intent.EnrollmentDigest != enrollment || c.IntentDigest != digest(record.Intent) || c.ObserverCertificateDigest != intent.ObserverCertificateDigest || c.NodeUID != intent.NodeUID || c.NodeBootID != intent.NodeBootID || c.IntentCreateRevision != c.IntentModRevision {
		return p.NodeTerminationQuery{}, fmt.Errorf("retained observer command/intent mismatch")
	}
	query := p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: c.Partition, TaskID: c.Current.TaskID, CommandID: c.Current.CommandID, IntentCreateRevision: c.IntentCreateRevision, IntentDigest: c.IntentDigest, EnrollmentDigest: enrollment}
	if _, err := p.EncodeNodeTerminationQuery(query); err != nil {
		return p.NodeTerminationQuery{}, err
	}
	return query, nil
}

func unknownResult(state string) p.NodeTerminationResult {
	return p.NodeTerminationResult{Version: 1, Purpose: "task_runtime_termination_result", State: state}
}

func (o *Observer) query(query p.NodeTerminationQuery) (p.NodeTerminationResult, error) {
	if _, err := p.EncodeNodeTerminationQuery(query); err != nil {
		return p.NodeTerminationResult{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.slots {
		if o.slots[i].registration.Version == 0 || digest(o.slots[i].wire) != query.EnrollmentDigest {
			continue
		}
		record := o.journal.records[i]
		if record.State == "enrolled" {
			return unknownResult("absent"), nil
		}
		original, err := retainedQuery(record, query.EnrollmentDigest)
		if err != nil {
			return p.NodeTerminationResult{}, err
		}
		if original != query {
			return unknownResult("absent"), nil
		}
		if record.State == "terminal" {
			return p.NodeTerminationResult{Version: 1, Purpose: "task_runtime_termination_result", State: "terminal", Proof: bytes.Clone(record.Terminal), ObserverCertificate: bytes.Clone(o.slots[i].registration.ObserverCertificate)}, nil
		}
		return unknownResult("unknown"), nil
	}
	return unknownResult("absent"), nil
}

func (o *Observer) terminate(ctx context.Context, wire, issuer []byte) (p.NodeTerminationResult, error) {
	if len(wire) == 0 || len(wire) > 8192 {
		return p.NodeTerminationResult{}, fmt.Errorf("observer ticket exceeds bound")
	}
	var ticket struct {
		Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
	}
	if err := json.Unmarshal(wire, &ticket); err != nil {
		return p.NodeTerminationResult{}, err
	}
	c := ticket.Claims
	if digest(issuer) != c.Current.IssuerCertificateDigest {
		return p.NodeTerminationResult{}, fmt.Errorf("observer TLS issuer does not match command")
	}
	query := p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: c.Partition, TaskID: c.Current.TaskID, CommandID: c.Current.CommandID, IntentCreateRevision: c.IntentCreateRevision, IntentDigest: c.IntentDigest, EnrollmentDigest: c.EnrollmentDigest}
	if _, err := p.EncodeNodeTerminationQuery(query); err != nil {
		return p.NodeTerminationResult{}, err
	}
	o.mu.Lock()
	if o.stopping {
		o.mu.Unlock()
		return p.NodeTerminationResult{}, fmt.Errorf("observer is closing")
	}
	slot := -1
	for i := range o.slots {
		if o.slots[i].registration.Version == 1 && digest(o.slots[i].wire) == c.EnrollmentDigest {
			slot = i
			break
		}
	}
	if slot < 0 {
		o.mu.Unlock()
		return unknownResult("absent"), nil
	}
	entry := &o.slots[slot]
	record := o.journal.records[slot]
	if record.State != "enrolled" || entry.running {
		original := record.Ticket
		if entry.running && record.State == "enrolled" {
			original = entry.pending
		}
		if !bytes.Equal(original, wire) {
			o.mu.Unlock()
			return p.NodeTerminationResult{}, fmt.Errorf("original observer command cannot be replaced")
		}
		done := entry.ownerDone
		running := entry.running
		o.mu.Unlock()
		if running {
			select {
			case <-done:
			case <-ctx.Done():
				return unknownResult("unknown"), ctx.Err()
			}
		}
		return o.query(query)
	}
	if entry.anchor == nil {
		o.mu.Unlock()
		return unknownResult("unknown"), nil
	}
	select {
	case o.active <- struct{}{}:
	default:
		o.mu.Unlock()
		return p.NodeTerminationResult{}, fmt.Errorf("observer termination worker budget full")
	}
	entry.running, entry.ownerDone, entry.pending = true, make(chan struct{}), bytes.Clone(wire)
	done := entry.ownerDone
	o.terminationWG.Add(1)
	o.mu.Unlock()
	go func() {
		defer o.terminationWG.Done()
		defer func() {
			o.mu.Lock()
			entry.running, entry.pending = false, nil
			close(done)
			o.mu.Unlock()
			<-o.active
		}()
		if err := o.executeTermination(uint32(slot), wire, issuer); err != nil {
			slog.Warn("node observer termination remains unresolved", "slot", slot, "reason", err)
		}
	}()
	select {
	case <-done:
		return o.query(query)
	case <-ctx.Done():
		return unknownResult("unknown"), ctx.Err()
	}
}

func (o *Observer) executeTermination(slot uint32, wire, issuer []byte) error {
	ctx, cancel := context.WithTimeout(o.ctx, o.options.TerminationTimeout)
	defer cancel()
	entry := &o.slots[slot]
	registration := entry.registration
	activation, err := o.verifier.VerifyNodeEnrollmentRequest(registration.Request, registration.ObservedAt)
	if err != nil {
		return err
	}
	enrollment := authorityEnrollment{Identity: activation.Identity(), CreationIntentID: registration.Provider.CreationIntentID, EnrollmentDigest: digest(entry.wire), ObserverCertificateDigest: digest(registration.ObserverCertificate), NodeUID: registration.Provider.NodeUID, NodeBootID: registration.Provider.NodeBootID}
	cutoff, _ := ctx.Deadline()
	snapshot, err := o.authority.load(ctx, wire, issuer, enrollment, cutoff)
	if err != nil {
		return err
	}
	provider := providerRegistration{Identity: activation.Identity(), Birth: activation.Birth(), CreationIntentID: registration.Provider.CreationIntentID}
	if _, err := o.provider.observe(ctx, provider, true); err != nil {
		return err
	}
	now, err := o.observe(ctx)
	if err != nil {
		return err
	}
	if _, err := o.verifier.VerifyNodeObserverCertificate(o.options.Certificate, o.options.NodeUID, o.identity.NodeBootID(), now.UTC); err != nil {
		return err
	}
	certificateCutoff := time.Now().Add(o.identity.NotAfter().Sub(now.UTC) - now.Uncertainty)
	if certificateCutoff.Before(snapshot.cutoff) {
		snapshot.cutoff = certificateCutoff
	}
	anchor := entry.anchor
	if err := anchor.revalidate(); err != nil {
		return err
	}
	if err := validateProcessBoundary(anchor); err != nil {
		return err
	}
	if err := snapshot.revalidate(ctx); err != nil {
		return err
	}
	record := journalRecord{Version: 1, State: "accepted", Enrollment: bytes.Clone(entry.wire), Intent: bytes.Clone(snapshot.intent.Value), Ticket: bytes.Clone(wire)}
	preflight := record
	preflight.State, preflight.Terminal = "terminal", json.RawMessage(`"`+strings.Repeat("x", 4094)+`"`)
	if _, err := journalPayload(preflight); err != nil {
		return err
	}
	o.mu.Lock()
	if err := snapshot.live(ctx); err != nil || o.stopping {
		o.mu.Unlock()
		return fmt.Errorf("observer acceptance cutoff lost")
	}
	err = o.journal.put(slot, record)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	// This durable write-ahead state consumes the sole syscall attempt. Every
	// later refusal, timeout, lost ACK or restart remains unknown, never retry.
	record.State = "signal-issued"
	o.mu.Lock()
	err = o.journal.put(slot, record)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := o.provider.observe(ctx, provider, true); err != nil {
		return err
	}
	if err := snapshot.revalidate(ctx); err != nil {
		return err
	}
	if err := anchor.revalidate(); err != nil {
		return err
	}
	if boot, err := bootID(); err != nil || boot != registration.Provider.NodeBootID {
		return fmt.Errorf("observer host boot changed before signal")
	}
	if err := snapshot.live(ctx); err != nil {
		return err
	}
	if err := anchor.kill(); err != nil {
		return err
	}
	if err := anchor.waitDeathAndEmpty(ctx); err != nil {
		return err
	}
	observed, err := o.provider.observe(ctx, provider, true)
	if err != nil {
		return err
	}
	if err := snapshot.revalidate(ctx); err != nil {
		return err
	}
	if empty, err := anchor.cgroup.empty(); err != nil || !empty {
		return fmt.Errorf("original cgroup no longer empty")
	}
	now, err = o.observe(ctx)
	if err != nil {
		return err
	}
	if err := snapshot.live(ctx); err != nil {
		return err
	}
	c := snapshot.ticket.Claims()
	proof, err := p.SignNodeTerminationProof(o.options.PrivateKey, o.identity, p.NodeTerminationProofClaims{Version: 1, Purpose: "task_runtime_terminated", Current: c.Current, Partition: c.Partition, IntentCreateRevision: c.IntentCreateRevision, IntentDigest: c.IntentDigest, EnrollmentDigest: c.EnrollmentDigest, ObserverCertificateDigest: c.ObserverCertificateDigest, TicketDigest: snapshot.ticket.Digest(), NodeUID: c.NodeUID, NodeBootID: c.NodeBootID, PodUID: observed.PodUID, PodResourceVersion: observed.ResourceVersion, ProviderDeletingAt: observed.DeletingAt, PID1Dead: true, CgroupEmpty: true, ObservedAt: now.UTC})
	if err != nil {
		return err
	}
	record.State, record.Terminal = "terminal", proof
	o.mu.Lock()
	err = o.journal.put(slot, record)
	if err == nil {
		err = anchor.close()
		entry.anchor = nil
	}
	o.mu.Unlock()
	return err
}

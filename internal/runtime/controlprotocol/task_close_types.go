package controlprotocol

import (
	"fmt"
	"time"
)

// TaskCloseDataContext pins original task/claim and exact runtime attribution.
// Trusted consumers supply it independently; it carries no business expiry or
// live metadata, transport, gate, drain or owner-release capability.
type TaskCloseDataContext struct {
	Namespace               string           `json:"namespace"`
	AuthorityID             string           `json:"authority_id"`
	Target                  string           `json:"target"`
	RestoreEpoch            string           `json:"restore_epoch"`
	IssuerCertificateID     string           `json:"issuer_certificate_id"`
	IssuerCertificateDigest string           `json:"issuer_certificate_digest"`
	CommandID               string           `json:"command_id"`
	TaskID                  string           `json:"task_id"`
	TaskDigest              string           `json:"task_digest"`
	ClaimID                 string           `json:"claim_id"`
	WorkerID                string           `json:"worker_id"`
	SandboxID               string           `json:"sandbox_id"`
	WorkspaceHash           string           `json:"workspace_hash"`
	Generation              int64            `json:"generation"`
	DataGateEpoch           int64            `json:"data_gate_epoch"`
	ControlRevision         int64            `json:"control_revision"`
	ClaimCreateRevision     int64            `json:"claim_create_revision"`
	LeaseID                 int64            `json:"lease_id"`
	Runtime                 RuntimeReference `json:"runtime"`
}

type TaskCloseDataTicketClaims struct {
	Version   uint32               `json:"version"`
	Purpose   string               `json:"purpose"`
	Context   TaskCloseDataContext `json:"context"`
	NotBefore time.Time            `json:"not_before"`
	NotAfter  time.Time            `json:"not_after"`
}

// TaskCloseDataEvidence is copied signature attribution, not live authority.
type TaskCloseDataEvidence struct {
	wire   []byte
	digest string
	claims TaskCloseDataTicketClaims
}

func (e TaskCloseDataEvidence) Wire() []byte                  { return append([]byte(nil), e.wire...) }
func (e TaskCloseDataEvidence) Digest() string                { return e.digest }
func (e TaskCloseDataEvidence) Context() TaskCloseDataContext { return e.claims.Context }
func (e TaskCloseDataEvidence) NotBefore() time.Time          { return e.claims.NotBefore }
func (e TaskCloseDataEvidence) NotAfter() time.Time           { return e.claims.NotAfter }

type TaskDataClosedReceiptClaims struct {
	Version      uint32               `json:"version"`
	State        string               `json:"state"`
	Context      TaskCloseDataContext `json:"context"`
	TicketDigest string               `json:"ticket_digest"`
	NotBefore    time.Time            `json:"not_before"`
	NotAfter     time.Time            `json:"not_after"`
}

// TaskDataClosedReceiptEvidence attributes a local durable admission closure.
// It says nothing about accepted commands, drain, settlement or safe release.
type TaskDataClosedReceiptEvidence struct {
	wire   []byte
	claims TaskDataClosedReceiptClaims
}

func (e TaskDataClosedReceiptEvidence) Wire() []byte                  { return append([]byte(nil), e.wire...) }
func (e TaskDataClosedReceiptEvidence) Context() TaskCloseDataContext { return e.claims.Context }
func (e TaskDataClosedReceiptEvidence) TicketDigest() string          { return e.claims.TicketDigest }
func (e TaskDataClosedReceiptEvidence) NotBefore() time.Time          { return e.claims.NotBefore }
func (e TaskDataClosedReceiptEvidence) NotAfter() time.Time           { return e.claims.NotAfter }
func (e TaskDataClosedReceiptEvidence) State() string                 { return e.claims.State }

var taskCloseContextSchema = objectSchema(map[string]*wireSchema{
	"namespace": stringField, "authority_id": stringField, "target": stringField, "restore_epoch": stringField,
	"issuer_certificate_id": stringField, "issuer_certificate_digest": stringField, "command_id": stringField,
	"task_id": stringField, "task_digest": stringField, "claim_id": stringField, "worker_id": stringField,
	"sandbox_id": stringField, "workspace_hash": stringField, "generation": numberField, "data_gate_epoch": numberField,
	"control_revision": numberField, "claim_create_revision": numberField, "lease_id": numberField, "runtime": runtimeSchema,
})

func taskCloseBinding(c TaskCloseDataContext) TrustBinding {
	return TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}
}
func validateTaskCloseContext(c TaskCloseDataContext) error {
	if err := validateBinding(taskCloseBinding(c)); err != nil {
		return err
	}
	if !validUUID(c.IssuerCertificateID) || !validHash(c.IssuerCertificateDigest) || !validUUID(c.CommandID) || !validUUID(c.TaskID) || !validHash(c.TaskDigest) || !validUUID(c.ClaimID) || !validID(c.WorkerID) || !validID(c.SandboxID) || !validHash(c.WorkspaceHash) || c.Generation <= 0 || c.DataGateEpoch <= 0 || c.ControlRevision <= 0 || c.ClaimCreateRevision <= c.ControlRevision || c.LeaseID <= 0 || !validRuntime(c.Runtime) {
		return fmt.Errorf("invalid task close context")
	}
	return nil
}
func validateTaskCloseWindow(before, after time.Time) error {
	if !validUTC(before) || !validUTC(after) || !before.Before(after) || after.Sub(before) > 30*time.Second {
		return fmt.Errorf("invalid task close interval")
	}
	return nil
}

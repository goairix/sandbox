package nodeobserver

import (
	"bytes"
	"encoding/json"
	"fmt"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// These are fixed historical q/none store wires. They intentionally do not
// reproduce the cleanup execution schema, which belongs to Task3's native
// writer. Historical signatures authorize no new runtime send or fresh Claim.
type detachedHistoricalClaim struct {
	Task           authorityTaskReference `json:"task"`
	ClaimID        string                 `json:"claim_id"`
	WorkerID       string                 `json:"worker_id"`
	CreateRevision int64                  `json:"create_revision"`
	LeaseID        int64                  `json:"lease_id"`
}

func (c detachedHistoricalClaim) matches(ref authorityTaskReference, claim p.DetachedNoEffectClaim) bool {
	return c.Task == ref && c.ClaimID == claim.ClaimID && c.WorkerID == claim.WorkerID && c.CreateRevision == claim.CreateRevision && c.LeaseID == claim.LeaseID
}

type detachedQuiescenceRecord struct {
	Version                 uint32                      `json:"version"`
	Task                    authorityTaskRecord         `json:"task"`
	Claim                   detachedHistoricalClaim     `json:"claim"`
	Context                 p.TaskUserQuiescenceContext `json:"context"`
	TicketDigest            string                      `json:"ticket_digest"`
	IssuerCertificateID     string                      `json:"issuer_certificate_id"`
	IssuerCertificateDigest string                      `json:"issuer_certificate_digest"`
	IssuerRevision          int64                       `json:"issuer_revision"`
	Ticket                  json.RawMessage             `json:"ticket"`
	CloseDataReceipt        json.RawMessage             `json:"close_data_receipt"`
	Attempt                 authorityStage              `json:"attempt"`
}

type detachedStageReference struct {
	Namespace    string `json:"namespace"`
	Partition    uint8  `json:"partition"`
	RequestID    string `json:"request_id"`
	StageID      string `json:"stage_id"`
	AttemptID    string `json:"attempt_id"`
	Digest       string `json:"digest"`
	RestoreEpoch string `json:"restore_epoch"`
}

type detachedQuiescenceReservation struct {
	Version            uint32                  `json:"version"`
	Task               authorityTaskRecord     `json:"task"`
	Claim              detachedHistoricalClaim `json:"claim"`
	DestroyingRevision int64                   `json:"destroying_revision"`
	CommandID          string                  `json:"command_id"`
	Attempt            authorityStage          `json:"attempt"`
}

type detachedFinalSyncRecord struct {
	Version            uint32                  `json:"version"`
	Task               authorityTaskRecord     `json:"task"`
	Claim              detachedHistoricalClaim `json:"claim"`
	DestroyingRevision int64                   `json:"destroying_revision"`
	Quiescence         struct {
		Task      authorityTaskReference `json:"task"`
		CommandID string                 `json:"command_id"`
		Stage     detachedStageReference `json:"stage"`
	} `json:"quiescence"`
	QuiescenceRevision int64  `json:"quiescence_revision"`
	ReceiptDigest      string `json:"receipt_digest"`
	Plan               struct {
		Mode   string `json:"mode"`
		Digest string `json:"digest"`
	} `json:"plan"`
	AttemptID string `json:"attempt_id"`
}

func detachedStageFor(s *authoritySnapshot) detachedStageReference {
	c, stage := s.record.Current, s.record.Authority.QuiescenceStage
	return detachedStageReference{c.Namespace, s.record.Partition, stage.RequestID, stage.StageID, stage.AttemptID, stage.Digest, c.RestoreEpoch}
}

func (r *authorityReader) authenticateDetachedQuiescence(s *authoritySnapshot, task authorityTaskRecord, issuer []byte) error {
	fact := s.detached.record
	var q detachedQuiescenceRecord
	if canonicalNative(s.history[0].Value, &q) != nil || q.Version != 1 || q.Task != task || q.Context.Current != fact.QuiescenceContext || q.IssuerCertificateID != fact.QuiescenceContext.IssuerCertificateID || q.IssuerCertificateDigest != digest(issuer) || q.IssuerRevision != s.history[4].CreateRevision {
		return fmt.Errorf("observer detached historical quiescence Task/context mismatch")
	}
	c := q.Context.Current
	claim := p.DetachedNoEffectClaim{ClaimID: c.ClaimID, WorkerID: c.WorkerID, CreateRevision: c.ClaimCreateRevision, LeaseID: c.LeaseID}
	stage := detachedStageFor(s)
	attempt := authorityStage{stage.Namespace, stage.Partition, stage.RequestID, stage.StageID, stage.AttemptID, stage.RestoreEpoch}
	if !q.Claim.matches(task.Reference, claim) || q.Attempt != attempt || q.Attempt.RequestID != c.ClaimID || q.Attempt.StageID != "task_quiesce_users" || s.history[0].CreateRevision <= c.ClaimCreateRevision || s.history[0].CreateRevision <= q.IssuerRevision || s.history[0].CreateRevision <= q.Context.CloseDataIntentRevision || digest(q.Ticket) != q.TicketDigest || digest(q.CloseDataReceipt) != q.Context.CloseDataReceiptDigest {
		return fmt.Errorf("observer detached historical quiescence attribution invalid")
	}
	var receipt struct {
		Version uint32 `json:"version"`
		detachedStageReference
		Outcome string `json:"outcome"`
	}
	var reservation detachedQuiescenceReservation
	if canonicalEnrolledNative(s.history[1].Value, &receipt) != nil || receipt.Version != 1 || receipt.Outcome != "committed" || receipt.detachedStageReference != stage || s.history[1].CreateRevision != s.history[0].CreateRevision || canonicalEnrolledNative(s.history[2].Value, &reservation) != nil || reservation.Version != 1 || reservation.Task != task || reservation.Claim != q.Claim || reservation.DestroyingRevision != fact.TaskBirth || reservation.CommandID != c.CommandID || reservation.Attempt != q.Attempt || s.history[2].CreateRevision <= c.ClaimCreateRevision || s.history[2].CreateRevision >= s.history[0].CreateRevision {
		return fmt.Errorf("observer detached original quiescence reservation/receipt invalid")
	}
	var final detachedFinalSyncRecord
	if canonicalEnrolledNative(s.history[3].Value, &final) != nil || final.Version != 1 || final.Task != task || !final.Claim.matches(task.Reference, fact.FinalSyncClaim) || final.DestroyingRevision != fact.TaskBirth || final.Quiescence.Task != task.Reference || final.Quiescence.CommandID != c.CommandID || final.Quiescence.Stage != stage || final.QuiescenceRevision != s.history[0].CreateRevision || final.ReceiptDigest != fact.ReceiptDigest || final.Plan.Mode != "none" || !authorityHash(final.Plan.Digest) || final.AttemptID != fact.FinalSyncAttemptID || s.history[3].CreateRevision <= final.Claim.CreateRevision || s.history[3].CreateRevision <= final.QuiescenceRevision || !detachedClaimBefore(claim, p.TaskCloseDataContext{ClaimCreateRevision: fact.FinalSyncClaim.CreateRevision, ClaimID: fact.FinalSyncClaim.ClaimID, WorkerID: fact.FinalSyncClaim.WorkerID, LeaseID: fact.FinalSyncClaim.LeaseID}) {
		return fmt.Errorf("observer detached original final-sync none history invalid")
	}
	// The original runtime users-quiesced receipt is authenticated by Task3's
	// actual dual-drain Query before it writes the permanent fact. Its digest
	// remains independently bound to this immutable original none reservation.
	return nil
}

func (r *authorityReader) authenticateDetachedHistoricalTickets(s *authoritySnapshot, runtimeCertificate []byte) error {
	var q detachedQuiescenceRecord
	if canonicalNative(s.history[0].Value, &q) != nil {
		return fmt.Errorf("observer detached quiescence ticket record invalid")
	}
	var issuer struct {
		Version           uint32          `json:"version"`
		Namespace         string          `json:"namespace"`
		RestoreEpoch      string          `json:"restore_epoch"`
		CertificateID     string          `json:"certificate_id"`
		CertificateDigest string          `json:"certificate_digest"`
		Certificate       json.RawMessage `json:"certificate"`
	}
	if canonicalNative(s.history[4].Value, &issuer) != nil || digest(issuer.Certificate) != q.IssuerCertificateDigest {
		return fmt.Errorf("observer detached historical issuer invalid")
	}
	var ticket struct {
		Claims p.TaskUserQuiescenceTicketClaims `json:"claims"`
	}
	if json.Unmarshal(q.Ticket, &ticket) != nil {
		return fmt.Errorf("observer detached historical q ticket missing")
	}
	at := ticket.Claims.NotBefore.Add(ticket.Claims.NotAfter.Sub(ticket.Claims.NotBefore) / 2)
	verified, err := r.verifier.VerifyTaskUserQuiescenceTicket(q.Ticket, issuer.Certificate, q.Context, at)
	if err != nil || verified.Digest() != q.TicketDigest {
		return fmt.Errorf("observer detached historical q ticket invalid: %v", err)
	}
	var closeReceipt struct {
		Claims p.TaskDataClosedReceiptClaims `json:"claims"`
	}
	if json.Unmarshal(q.CloseDataReceipt, &closeReceipt) != nil {
		return fmt.Errorf("observer detached historical close receipt missing")
	}
	claims := closeReceipt.Claims
	at = claims.NotBefore.Add(claims.NotAfter.Sub(claims.NotBefore) / 2)
	closed, err := r.verifier.VerifyTaskDataClosedReceipt(q.CloseDataReceipt, runtimeCertificate, q.Context.CloseDataContext, q.Context.CloseDataTicketDigest, claims.NotBefore, claims.NotAfter, at)
	if err != nil || !bytes.Equal(closed.Wire(), q.CloseDataReceipt) {
		return fmt.Errorf("observer detached historical close receipt invalid: %v", err)
	}
	return nil
}

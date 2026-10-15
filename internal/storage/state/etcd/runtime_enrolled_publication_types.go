package etcd

import (
	"encoding/json"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

type EnrolledPublicationOptions struct {
	ContractDigest string
	MaxActive      uint32
}

type RuntimeEnrolledPublishResult struct {
	Outcome           Outcome
	Disposition       RuntimePublishDisposition
	Reference         StageReference
	Entry             *RuntimeEnrolledPublicationEntry
	GuardCleanupError error
}

// RuntimeEnrolledPublicationEntry is copied historical evidence only.
type RuntimeEnrolledPublicationEntry struct {
	Record     RuntimeEnrolledPublicationRecord
	Proof      json.RawMessage
	Enrollment controlprotocol.EnrolledRuntimeEvidence
	Reference  StageReference
}

type RuntimeEnrolledPublicationRecord struct {
	RuntimePublicationRecord
	EnrollmentDigest          string              `json:"enrollment_digest"`
	ObserverCertificateDigest string              `json:"observer_certificate_digest"`
	ActivationDigest          string              `json:"activation_digest"`
	NodeUID                   string              `json:"node_uid"`
	NodeBootID                string              `json:"node_boot_id"`
	EnrollmentRecordDigest    string              `json:"enrollment_record_digest"`
	ActivationRecordDigest    string              `json:"activation_record_digest"`
	DispatchAttempt           StageAttemptLocator `json:"dispatch_attempt"`
	BindingAttempt            StageAttemptLocator `json:"binding_attempt"`
	MountAttemptLocator       StageAttemptLocator `json:"mount_attempt_locator"`
}

type RuntimeEnrolledPublicationProofRecord struct {
	RuntimePublicationProofRecord
}

type RuntimeEnrollmentRecord struct {
	Version             uint32           `json:"version"`
	IntentID            string           `json:"intent_id"`
	SandboxID           string           `json:"sandbox_id"`
	WorkspaceHash       string           `json:"workspace_hash"`
	RestoreEpoch        string           `json:"restore_epoch"`
	Generation          int64            `json:"generation"`
	Runtime             RuntimeReference `json:"runtime"`
	ObserverCertificate json.RawMessage  `json:"observer_certificate"`
	Receipt             json.RawMessage  `json:"receipt"`
}

type RuntimeEnrollmentActivationRecord struct {
	Version            uint32           `json:"version"`
	IntentID           string           `json:"intent_id"`
	SandboxID          string           `json:"sandbox_id"`
	WorkspaceHash      string           `json:"workspace_hash"`
	RestoreEpoch       string           `json:"restore_epoch"`
	Generation         int64            `json:"generation"`
	Runtime            RuntimeReference `json:"runtime"`
	Activation         json.RawMessage  `json:"activation"`
	RuntimeCertificate json.RawMessage  `json:"runtime_certificate"`
	IssuerCertificate  json.RawMessage  `json:"issuer_certificate"`
}

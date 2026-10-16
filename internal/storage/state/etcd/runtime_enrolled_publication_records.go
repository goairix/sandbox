package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

const enrolledPublicationStage = "runtime_publish_enrolled_v2"
const enrolledDuePublicationStage = "runtime_publish_enrolled_v3"

func supportedEnrolledPublicationStage(stage string) bool {
	return stage == enrolledPublicationStage || stage == enrolledDuePublicationStage
}

func (r RuntimeEnrolledPublicationRecord) Validate() error {
	legacy := r.RuntimePublicationRecord
	legacy.Version = 1
	legacy.Attempt.StageID = "runtime_publish"
	versionStage := r.Version == 2 && r.Attempt.StageID == enrolledPublicationStage || r.Version == 3 && r.Attempt.StageID == enrolledDuePublicationStage
	if !versionStage || legacy.Validate() != nil || r.MountAttempt != 0 || r.MountOperationID != "" || !validHexDigest(r.EnrollmentDigest) || !validHexDigest(r.ObserverCertificateDigest) || !validHexDigest(r.ActivationDigest) || !validHexDigest(r.EnrollmentRecordDigest) || !validHexDigest(r.ActivationRecordDigest) || !validPreparationUUID(r.NodeUID) || !validPreparationUUID(r.NodeBootID) {
		return ErrInvalidRecord
	}
	for _, pin := range []struct {
		attempt StageAttemptLocator
		stage   string
	}{{r.DispatchAttempt, "runtime_dispatch"}, {r.BindingAttempt, "runtime_bind"}, {r.MountAttemptLocator, "runtime_mount"}} {
		if !validPreparationAttempt(pin.attempt, r.WorkspaceHash, r.RestoreEpoch, pin.stage) || pin.attempt.Namespace != r.Attempt.Namespace || pin.attempt.Partition != r.Attempt.Partition {
			return ErrInvalidRecord
		}
	}
	return nil
}

func (r RuntimeEnrolledPublicationProofRecord) Validate() error {
	legacy := r.RuntimePublicationProofRecord
	legacy.Version = 1
	if r.Version != 2 {
		return ErrInvalidRecord
	}
	return legacy.Validate()
}

func validEnrolledObject(w json.RawMessage) bool {
	return len(w) > 1 && w[0] == '{' && w[len(w)-1] == '}' && json.Valid(w)
}

func validEnrolledIdentity(version uint32, hash, sandbox, intent, restore string, generation int64, runtime RuntimeReference) bool {
	return version == 2 && validOwnership(1, hash, sandbox, intent, restore, generation) && runtime.Validate() == nil
}

func (r RuntimeEnrollmentRecord) Validate() error {
	if !validEnrolledIdentity(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation, r.Runtime) || !validEnrolledObject(r.ObserverCertificate) || !validEnrolledObject(r.Receipt) {
		return ErrInvalidRecord
	}
	return nil
}

func (r RuntimeEnrollmentActivationRecord) Validate() error {
	if !validEnrolledIdentity(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation, r.Runtime) || !validEnrolledObject(r.Activation) || !validEnrolledObject(r.RuntimeCertificate) || !validEnrolledObject(r.IssuerCertificate) {
		return ErrInvalidRecord
	}
	return nil
}

func encodeEnrolledRecord(record interface{ Validate() error }) (string, error) {
	if err := record.Validate(); err != nil {
		return "", err
	}
	wire, err := json.Marshal(record)
	if err != nil || len(wire) > enrolledRecordLimit(record) {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

func decodeEnrolledRecord(wire []byte, record interface{ Validate() error }) error {
	if len(wire) == 0 || len(wire) > enrolledRecordLimit(record) {
		return ErrCorruptRecord
	}
	if err := strictPreparationMetadata(wire, reflect.TypeOf(record).Elem()); err != nil {
		return ErrCorruptRecord
	}
	if err := json.Unmarshal(wire, record); err != nil {
		return ErrCorruptRecord
	}
	canonical, err := encodeEnrolledRecord(record)
	if err != nil || !bytes.Equal(wire, []byte(canonical)) {
		return ErrCorruptRecord
	}
	return nil
}

func enrolledRecordLimit(record interface{ Validate() error }) int {
	switch record.(type) {
	case RuntimeEnrollmentRecord, *RuntimeEnrollmentRecord, RuntimeEnrollmentActivationRecord, *RuntimeEnrollmentActivationRecord, RuntimeEnrolledPublicationRecord, *RuntimeEnrolledPublicationRecord:
		return 8192
	case RuntimeEnrolledPublicationProofRecord, *RuntimeEnrolledPublicationProofRecord:
		return 16384
	default:
		return maxRecordBytes
	}
}

func (n Namespace) runtimeEnrollmentKeys(partition uint8, intent string) (string, string, error) {
	if !validDomainSegment(intent) {
		return "", "", ErrInvalidRecord
	}
	e, err := n.Key("p", fmt.Sprintf("%02x", partition), "intents", intent, "runtime-enrollment")
	if err != nil {
		return "", "", err
	}
	a, err := n.Key("p", fmt.Sprintf("%02x", partition), "intents", intent, "runtime-enrollment-activation")
	return e, a, err
}

func enrollmentRecords(d RuntimeDispatchRecord, enrollment controlprotocol.EnrolledRuntimeEvidence) (RuntimeEnrollmentRecord, RuntimeEnrollmentActivationRecord) {
	// The caller has already authenticated this exact material and joins the
	// independently certified identities before any native request.
	var receipt controlprotocol.NodeEnrollmentReceipt
	_ = json.Unmarshal(enrollment.Receipt, &receipt)
	runtime := RuntimeReference(receipt.Claims.Runtime)
	e := RuntimeEnrollmentRecord{2, d.IntentID, d.SandboxID, d.WorkspaceHash, d.RestoreEpoch, d.Generation, runtime, bytes.Clone(enrollment.ObserverCertificate), bytes.Clone(enrollment.Receipt)}
	a := RuntimeEnrollmentActivationRecord{2, d.IntentID, d.SandboxID, d.WorkspaceHash, d.RestoreEpoch, d.Generation, runtime, bytes.Clone(enrollment.Request.Activation), bytes.Clone(enrollment.Request.RuntimeCertificate), bytes.Clone(enrollment.Request.IssuerCertificate)}
	return e, a
}

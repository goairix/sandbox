package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const detachedCleanupPlainImageForTest = "registry.example/sandbox:1.0.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type detachedCleanupSourceFixture struct {
	duePublicationFixture
	task        TaskRecord
	snapshot    SandboxSnapshotRecord
	intent      CreationIntentRecord
	publication RuntimeEnrolledPublicationEntry
}

// Every positive source comes from original detached acquisition and actual
// signed publication, then the ordinary destroy transaction creates its Task.
// This host fixture asserts metadata authentication, never physical cleanup.
func newDetachedCleanupSourceFixture(t *testing.T, profile string, options *DetachedCleanupOptions, configure ...func(*Options)) detachedCleanupSourceFixture {
	t.Helper()
	f := newDuePublicationFixtureForSource(t, profile, options, configure...)
	return detachedCleanupSourceOnPublication(t, f)
}

func detachedCleanupSourceOnPublication(t *testing.T, f duePublicationFixture) detachedCleanupSourceFixture {
	t.Helper()
	ctx := context.Background()
	published, err := f.b.PublishRuntimeEnrolled(ctx, f.claim, f.proof, f.enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	require.NotNil(t, published.Entry)

	controlKey, _, err := f.b.namespace.sandboxKeys(f.claim.reference.Partition, f.claim.reference.SandboxID, "read")
	require.NoError(t, err)
	control := f.server.point(controlKey)
	require.NotNil(t, control)
	stage, ref := taskDestroyPrepare(t, f.b, ctx, BeginDestroyInput{SandboxID: f.claim.reference.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: control.ModRevision})
	outcome, err := f.b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	task, err := f.b.LoadTask(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, task)
	snapshot, err := f.b.LoadSnapshot(ctx, ref.Partition, ref.SandboxID, task.Snapshot)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	intent, err := f.b.LoadCreationIntent(ctx, f.claim.workspace, task.CreationIntentID)
	require.NoError(t, err)
	require.NotNil(t, intent)
	require.Equal(t, "published", intent.Phase)
	return detachedCleanupSourceFixture{duePublicationFixture: f, task: *task, snapshot: *snapshot, intent: *intent, publication: *published.Entry}
}

func TestDetachedCleanupSourceAuthenticatesOriginalDetachedHistory(t *testing.T) {
	options := &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest}
	f := newDetachedCleanupSourceFixture(t, "detached", options)
	got, err := f.b.detachedCleanupSource(f.task, f.snapshot, f.intent, &f.publication)
	require.NoError(t, err)
	require.Equal(t, f.claim.workspace, got)
	require.Equal(t, WorkspaceIdentityDetached, got.Kind())

	// ConfigurationDigest additionally binds the original acquisition TTL,
	// which this classifier cannot reconstruct. Its codec is checked, while
	// durable writers must pin the original intent KV independently.
	intent := f.intent
	intent.ConfigurationDigest = strings.Repeat("f", 64)
	_, err = f.b.detachedCleanupSource(f.task, f.snapshot, intent, &f.publication)
	require.NoError(t, err)

	// A legitimate Task may retain a later TTL than its original certificate.
	// Historical signatures keep their own original publication expiry binding.
	task := f.task
	task.ExpiresAt = task.ExpiresAt.Add(time.Hour)
	got, err = f.b.detachedCleanupSource(task, f.snapshot, f.intent, &f.publication)
	require.NoError(t, err)
	require.Equal(t, f.claim.workspace, got)

	options.PinnedPlainImage = "mutated-after-New"
	got, err = f.b.detachedCleanupSource(f.task, f.snapshot, f.intent, &f.publication)
	require.NoError(t, err, "New must own its validated profile independently of caller options")
	require.Equal(t, f.claim.workspace, got)
}

func TestDetachedCleanupSourceRejectsMismatchedAttribution(t *testing.T) {
	f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest})
	for name, mutate := range map[string]func(*detachedCleanupSourceFixture){
		"request hash": func(f *detachedCleanupSourceFixture) {
			f.intent.RequestHash = strings.Repeat("f", 64)
		},
		"task id": func(f *detachedCleanupSourceFixture) {
			f.task.Reference.TaskID = "invalid"
		},
		"task namespace": func(f *detachedCleanupSourceFixture) {
			f.task.Reference.Namespace = "/sandbox/foreign/cell/"
		},
		"task restore": func(f *detachedCleanupSourceFixture) {
			f.task.Reference.RestoreEpoch = "other"
		},
		"task partition": func(f *detachedCleanupSourceFixture) {
			f.task.Reference.Partition++
		},
		"task sandbox": func(f *detachedCleanupSourceFixture) {
			f.task.Reference.SandboxID = "other"
		},
		"task intent": func(f *detachedCleanupSourceFixture) {
			f.task.CreationIntentID = "other"
		},
		"task workspace": func(f *detachedCleanupSourceFixture) {
			f.task.WorkspaceHash = strings.Repeat("f", 64)
		},
		"task runtime": func(f *detachedCleanupSourceFixture) {
			f.task.Runtime.UID = "other"
		},
		"task generation": func(f *detachedCleanupSourceFixture) {
			f.task.Generation++
		},
		"task gate": func(f *detachedCleanupSourceFixture) {
			f.task.DataGateEpoch++
		},
		"task snapshot": func(f *detachedCleanupSourceFixture) {
			f.task.Snapshot.Version = "other"
		},
		"snapshot sandbox": func(f *detachedCleanupSourceFixture) {
			f.snapshot.SandboxID = "other"
		},
		"snapshot payload": func(f *detachedCleanupSourceFixture) {
			f.snapshot.Payload = json.RawMessage(`{"changed":true}`)
		},
		"snapshot digest": func(f *detachedCleanupSourceFixture) {
			f.snapshot.Snapshot.Digest = strings.Repeat("f", 64)
		},
		"snapshot version": func(f *detachedCleanupSourceFixture) {
			f.snapshot.Version = 2
		},
		"local descriptor": func(f *detachedCleanupSourceFixture) {
			f.snapshot.LocalCleanup = &LocalCleanupDescriptor{}
		},
		"intent id": func(f *detachedCleanupSourceFixture) {
			f.intent.IntentID = "other"
		},
		"intent sandbox": func(f *detachedCleanupSourceFixture) {
			f.intent.SandboxID = "other"
		},
		"intent workspace": func(f *detachedCleanupSourceFixture) {
			f.intent.WorkspaceHash = strings.Repeat("f", 64)
		},
		"intent restore": func(f *detachedCleanupSourceFixture) {
			f.intent.RestoreEpoch = "other"
		},
		"intent generation": func(f *detachedCleanupSourceFixture) {
			f.intent.Generation++
		},
		"intent phase": func(f *detachedCleanupSourceFixture) {
			f.intent.Phase = "pending"
		},
		"publication sandbox": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.SandboxID = "other"
		},
		"publication intent": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.IntentID = "other"
		},
		"publication workspace": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.WorkspaceHash = strings.Repeat("f", 64)
		},
		"publication runtime": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.Runtime.BootID = "other"
		},
		"publication generation": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.Generation++
		},
		"publication gate": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.DataGateEpoch++
		},
		"publication snapshot": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.Snapshot.Digest = strings.Repeat("f", 64)
		},
		"publication namespace": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.Attempt.Namespace = "/sandbox/foreign/cell/"
		},
		"publication restore": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.RestoreEpoch = "other"
		},
		"publication mount": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.MountAttempt = 1
		},
		"publication proof digest": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.ProofDigest = strings.Repeat("f", 64)
		},
		"publication enrollment digest": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.EnrollmentDigest = strings.Repeat("f", 64)
		},
		"publication observer digest": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.ObserverCertificateDigest = strings.Repeat("f", 64)
		},
		"publication activation digest": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.ActivationDigest = strings.Repeat("f", 64)
		},
		"publication node": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.NodeUID = uuid.NewString()
		},
		"publication boot": func(f *detachedCleanupSourceFixture) {
			f.publication.Record.NodeBootID = uuid.NewString()
		},
		"publication receipt reference": func(f *detachedCleanupSourceFixture) {
			f.publication.Reference.AttemptID = uuid.NewString()
		},
		"missing proof": func(f *detachedCleanupSourceFixture) {
			f.publication.Proof = nil
		},
		"corrupt proof": func(f *detachedCleanupSourceFixture) {
			f.publication.Proof = json.RawMessage(`{}`)
		},
		"missing enrollment": func(f *detachedCleanupSourceFixture) {
			f.publication.Enrollment = p.EnrolledRuntimeEvidence{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := f
			changed.snapshot = *cloneSnapshotRecord(&f.snapshot)
			changed.publication.Proof = bytes.Clone(f.publication.Proof)
			changed.publication.Enrollment = f.publication.Enrollment.Clone()
			mutate(&changed)
			identity, err := f.b.detachedCleanupSource(changed.task, changed.snapshot, changed.intent, &changed.publication)
			require.Error(t, err)
			require.Equal(t, WorkspaceIdentity{}, identity, "rejected metadata cannot return a usable identity")
		})
	}
}

func TestDetachedCleanupSourceRequiresHistoricalSignature(t *testing.T) {
	f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest})
	entry := f.publication
	var envelope struct {
		Certificate json.RawMessage `json:"certificate"`
		Claims      json.RawMessage `json:"claims"`
		Signature   []byte          `json:"signature"`
	}
	require.NoError(t, json.Unmarshal(entry.Proof, &envelope))
	require.Len(t, envelope.Signature, ed25519.SignatureSize)
	envelope.Signature[0] ^= 1
	proof, err := json.Marshal(envelope)
	require.NoError(t, err)
	entry.Proof = proof
	entry.Record.ProofDigest, err = snapshotDigest(proof)
	require.NoError(t, err)
	require.NoError(t, entry.Record.Validate(), "copied metadata and proof digest are internally legal")
	_, err = f.b.detachedCleanupSource(f.task, f.snapshot, f.intent, &entry)
	require.Error(t, err, "matching DTO fields and digest cannot replace genuine signature authentication")

	// A genuine root-signed FUSE certificate with the exact same historical
	// identity is still the wrong profile, even before any filesystem effect.
	dispatch, err := f.b.LoadRuntimeDispatch(context.Background(), f.claim.workspace, f.intent.IntentID)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	fuse := preparationCertificate(t, f.b, dispatch.Record, root, "fuse", f.task.Runtime)
	entry = f.publication
	entry.Record.CertificateDigest, err = snapshotDigest(fuse)
	require.NoError(t, err)
	entry.Record.MountAttempt = 1
	entry.Record.MountOperationID = uuid.NewString()
	ready := p.ReadyReceiptClaims{Version: 1, CertificateDigest: entry.Record.CertificateDigest, Claim: p.ClaimReference{ClaimID: entry.Record.Claim.ClaimID, CreateRevision: entry.Record.Claim.CreateRevision, LeaseID: entry.Record.Claim.LeaseID}, DataGateEpoch: entry.Record.DataGateEpoch, GateState: "open", MountAttempt: 1, MountOperationID: entry.Record.MountOperationID, ObservedAt: f.now, ValidUntil: f.now.Add(5 * time.Second)}
	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	entry.Proof, err = p.SignReadyReceipt(runtimeKey, fuse, ready)
	require.NoError(t, err)
	entry.Record.ProofDigest, err = snapshotDigest(entry.Proof)
	require.NoError(t, err)
	_, err = f.b.publicationVerifier.VerifyHistorical(entry.Proof, p.PublicationContext{Certificate: f.b.preparationCertificateContext(dispatch.Record), Runtime: p.RuntimeReference(f.task.Runtime), CertificateDigest: entry.Record.CertificateDigest, Claim: ready.Claim, DataGateEpoch: ready.DataGateEpoch, MountAttempt: 1, MountOperationID: ready.MountOperationID})
	require.NoError(t, err, "the rejected FUSE history is genuinely root/delegate signed")
	_, err = f.b.detachedCleanupSource(f.task, f.snapshot, f.intent, &entry)
	require.Error(t, err)
}

func TestDetachedCleanupSourceRejectsDifferentSignedPlainContract(t *testing.T) {
	f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest})
	otherImage := "registry.example/sandbox:1.0.1@sha256:" + strings.Repeat("b", 64)
	other := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: otherImage})
	b := *f.b
	b.detachedCleanup = other.b.detachedCleanup
	b.enrolledPublication = other.b.enrolledPublication
	_, err := b.detachedCleanupSource(f.task, f.snapshot, f.intent, &f.publication)
	require.Error(t, err, "a different approved plain image cannot authenticate this original signed activation")
}

func TestDetachedCleanupSourceRejectsOtherStorageAndProfiles(t *testing.T) {
	for _, profile := range []string{"s3", "local"} {
		t.Run(profile, func(t *testing.T) {
			f := newDetachedCleanupSourceFixture(t, profile, &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest})
			if profile == "local" {
				require.Equal(t, uint32(2), f.snapshot.Version)
				require.NotNil(t, f.snapshot.LocalCleanup)
			}
			_, err := f.b.detachedCleanupSource(f.task, f.snapshot, f.intent, &f.publication)
			require.Error(t, err)
		})
	}
	f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest})
	var identity Identity
	require.NoError(t, json.Unmarshal([]byte(f.b.identityValue), &identity))
	identity.StorageID = "foreign-storage"
	wire, err := json.Marshal(identity)
	require.NoError(t, err)
	foreign := *f.b
	foreign.identityValue = string(wire)
	_, err = foreign.detachedCleanupSource(f.task, f.snapshot, f.intent, &f.publication)
	require.Error(t, err)
	_, err = f.b.detachedCleanupSource(f.task, f.snapshot, f.intent, nil)
	require.Error(t, err)
	var missing *Backend
	_, err = missing.detachedCleanupSource(f.task, f.snapshot, f.intent, &f.publication)
	require.Error(t, err)
}

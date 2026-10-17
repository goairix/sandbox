package controlprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Independent wire structs preserve legacy field order and construct detached
// input without letting production schema changes alter the expected bytes.
type terminationLegacyWire struct {
	Version                   uint32                       `json:"version"`
	Phase                     string                       `json:"phase"`
	Current                   TaskCloseDataContext         `json:"current"`
	Partition                 uint8                        `json:"partition"`
	TaskBirth                 int64                        `json:"task_birth"`
	CreationIntentID          string                       `json:"creation_intent_id"`
	MountAttempt              uint8                        `json:"mount_attempt"`
	EnrollmentDigest          string                       `json:"enrollment_digest"`
	ObserverCertificateDigest string                       `json:"observer_certificate_digest"`
	NodeUID                   string                       `json:"node_uid"`
	NodeBootID                string                       `json:"node_boot_id"`
	Action                    string                       `json:"action"`
	Authority                 TerminationAuthorityBindings `json:"authority"`
	TerminalProof             []byte                       `json:"terminal_proof,omitempty"`
	CleanupDigest             string                       `json:"cleanup_digest,omitempty"`
}

type terminationDetachedEffectsWire struct {
	Version        uint32      `json:"version"`
	Profile        string      `json:"profile"`
	ContractDigest string      `json:"contract_digest"`
	Settlement     RecordStamp `json:"settlement"`
}

type terminationDetachedWire struct {
	terminationLegacyWire
	DetachedEffects *terminationDetachedEffectsWire `json:"detached_effects,omitempty"`
}

func terminationLegacyFixture(t *testing.T) terminationLegacyWire {
	t.Helper()
	c := nodeTerminationIntentFixture(t)
	return terminationLegacyWire{
		Version: c.Version, Phase: c.Phase, Current: c.Current, Partition: c.Partition,
		TaskBirth: c.TaskBirth, CreationIntentID: c.CreationIntentID, MountAttempt: c.MountAttempt,
		EnrollmentDigest: c.EnrollmentDigest, ObserverCertificateDigest: c.ObserverCertificateDigest,
		NodeUID: c.NodeUID, NodeBootID: c.NodeBootID, Action: c.Action, Authority: c.Authority,
		TerminalProof: c.TerminalProof, CleanupDigest: c.CleanupDigest,
	}
}

func terminationDetachedFixture(t *testing.T) terminationDetachedWire {
	t.Helper()
	c := terminationLegacyFixture(t)
	c.Version = 2
	c.Authority.Archive = RecordStamp{}
	c.Authority.LocalComplete = RecordStamp{}
	return terminationDetachedWire{
		terminationLegacyWire: c,
		DetachedEffects: &terminationDetachedEffectsWire{
			Version: 1, Profile: "detached_enrolled_plain", ContractDigest: strings.Repeat("a", 64),
			// Settlement may predate a renewed claim. Codec pins are not the
			// native reader's proof of historical Task/Claim/Run attribution.
			Settlement: RecordStamp{CreateRevision: 2, ModRevision: 2, SHA256: strings.Repeat("b", 64)},
		},
	}
}

func terminationMarshal(t *testing.T, c any) []byte {
	t.Helper()
	w, err := json.Marshal(c)
	require.NoError(t, err)
	return w
}

func terminationRoundTrip(t *testing.T, w []byte) TaskRuntimeTerminationIntent {
	t.Helper()
	c, err := DecodeTaskRuntimeTerminationIntent(w)
	require.NoError(t, err)
	got, err := EncodeTaskRuntimeTerminationIntent(c)
	require.NoError(t, err)
	require.Equal(t, w, got)
	return c
}

func TestDetachedTerminationIntentRoundTrip(t *testing.T) {
	for _, phase := range []string{"reserved", "terminated", "cleanup-complete"} {
		t.Run(phase, func(t *testing.T) {
			c := terminationDetachedFixture(t)
			c.Phase = phase
			if phase != "reserved" {
				c.TerminalProof = bytes.Repeat([]byte{1}, 4096)
			}
			if phase == "cleanup-complete" {
				c.CleanupDigest = strings.Repeat("c", 64)
			}

			got := terminationRoundTrip(t, terminationMarshal(t, c))
			require.Equal(t, &DetachedRuntimeEffectBindings{
				Version: 1, Profile: "detached_enrolled_plain", ContractDigest: strings.Repeat("a", 64),
				Settlement: RecordStamp{CreateRevision: 2, ModRevision: 2, SHA256: strings.Repeat("b", 64)},
			}, got.DetachedEffects)
		})
	}
}

func TestDetachedTerminationRejectsLegacyNativeKeys(t *testing.T) {
	c := terminationRoundTrip(t, terminationMarshal(t, terminationDetachedFixture(t)))
	keys, err := TaskRuntimeTerminationKeysFor(c)
	require.Error(t, err)
	require.Equal(t, TaskRuntimeTerminationKeys{}, keys)

	// The wire variant does not open the local-only native consumer. Its old
	// key set remains unchanged until a detached reader is implemented.
	legacy := terminationRoundTrip(t, terminationMarshal(t, terminationLegacyFixture(t)))
	keys, err = TaskRuntimeTerminationKeysFor(legacy)
	require.NoError(t, err)
	task := legacy.Current.Namespace + "p/ab/tasks/" + legacy.Current.TaskID
	require.Equal(t, task+"/workspace-archive-command", keys.Archive)
	require.Equal(t, task+"/workspace-sync-local-complete", keys.LocalComplete)
}

func TestDetachedTerminationPreservesVersionOneBytes(t *testing.T) {
	for _, phase := range []string{"reserved", "terminated", "cleanup-complete"} {
		t.Run(phase, func(t *testing.T) {
			legacy := terminationLegacyFixture(t)
			legacy.Phase = phase
			legacy.MountAttempt = 1
			if phase != "reserved" {
				legacy.TerminalProof = []byte("observer proof")
			}
			if phase == "cleanup-complete" {
				legacy.CleanupDigest = strings.Repeat("c", 64)
			}

			// The independent legacy schema has no detached field, including
			// no null placeholder, so its exact bytes protect stored v1 digests.
			w := terminationMarshal(t, legacy)
			got := terminationRoundTrip(t, w)
			require.Equal(t, legacy.Authority, got.Authority)
			require.NotContains(t, string(w), "detached_effects")
		})
	}
}

func TestDetachedTerminationIntentRejectsInvalidVariants(t *testing.T) {
	changes := map[string]func(*terminationDetachedWire){
		"intent-zero":                  func(c *terminationDetachedWire) { c.Version = 0 },
		"intent-three":                 func(c *terminationDetachedWire) { c.Version = 3 },
		"version-one-with-tuple":       func(c *terminationDetachedWire) { c.Version = 1 },
		"missing-tuple":                func(c *terminationDetachedWire) { c.DetachedEffects = nil },
		"empty-tuple":                  func(c *terminationDetachedWire) { c.DetachedEffects = &terminationDetachedEffectsWire{} },
		"tuple-zero":                   func(c *terminationDetachedWire) { c.DetachedEffects.Version = 0 },
		"tuple-two":                    func(c *terminationDetachedWire) { c.DetachedEffects.Version = 2 },
		"profile-empty":                func(c *terminationDetachedWire) { c.DetachedEffects.Profile = "" },
		"profile-local":                func(c *terminationDetachedWire) { c.DetachedEffects.Profile = "local" },
		"profile-unknown":              func(c *terminationDetachedWire) { c.DetachedEffects.Profile = "detached_plain" },
		"contract-empty":               func(c *terminationDetachedWire) { c.DetachedEffects.ContractDigest = "" },
		"contract-short":               func(c *terminationDetachedWire) { c.DetachedEffects.ContractDigest = strings.Repeat("a", 63) },
		"contract-long":                func(c *terminationDetachedWire) { c.DetachedEffects.ContractDigest = strings.Repeat("a", 65) },
		"contract-upper":               func(c *terminationDetachedWire) { c.DetachedEffects.ContractDigest = strings.Repeat("A", 64) },
		"contract-nonhex":              func(c *terminationDetachedWire) { c.DetachedEffects.ContractDigest = strings.Repeat("g", 64) },
		"settlement-zero":              func(c *terminationDetachedWire) { c.DetachedEffects.Settlement = RecordStamp{} },
		"settlement-create-zero":       func(c *terminationDetachedWire) { c.DetachedEffects.Settlement.CreateRevision = 0 },
		"settlement-create-negative":   func(c *terminationDetachedWire) { c.DetachedEffects.Settlement.CreateRevision = -1 },
		"settlement-mod-before-create": func(c *terminationDetachedWire) { c.DetachedEffects.Settlement.ModRevision = 1 },
		"settlement-hash-empty":        func(c *terminationDetachedWire) { c.DetachedEffects.Settlement.SHA256 = "" },
		"settlement-hash-upper":        func(c *terminationDetachedWire) { c.DetachedEffects.Settlement.SHA256 = strings.Repeat("B", 64) },
		"settlement-hash-short":        func(c *terminationDetachedWire) { c.DetachedEffects.Settlement.SHA256 = strings.Repeat("b", 63) },
		"settlement-hash-nonhex":       func(c *terminationDetachedWire) { c.DetachedEffects.Settlement.SHA256 = strings.Repeat("g", 64) },
		"archive-present":              func(c *terminationDetachedWire) { c.Authority.Archive = c.Authority.Task },
		"archive-partial":              func(c *terminationDetachedWire) { c.Authority.Archive.ModRevision = 1 },
		"local-complete-present":       func(c *terminationDetachedWire) { c.Authority.LocalComplete = c.Authority.Task },
		"local-complete-partial":       func(c *terminationDetachedWire) { c.Authority.LocalComplete.SHA256 = strings.Repeat("c", 64) },
		"mount-attempt":                func(c *terminationDetachedWire) { c.MountAttempt = 1 },
		"partition":                    func(c *terminationDetachedWire) { c.Partition++ },
		"task-birth-zero":              func(c *terminationDetachedWire) { c.TaskBirth = 0 },
		"task-birth-after-claim":       func(c *terminationDetachedWire) { c.TaskBirth = c.Current.ClaimCreateRevision + 1 },
		"runtime-missing":              func(c *terminationDetachedWire) { c.Current.Runtime.UID = "" },
		"namespace-bound":              func(c *terminationDetachedWire) { c.Current.Namespace = "/" + strings.Repeat("n/", 256) },
		"creation-intent":              func(c *terminationDetachedWire) { c.CreationIntentID = "bad" },
		"enrollment":                   func(c *terminationDetachedWire) { c.EnrollmentDigest = "" },
		"observer":                     func(c *terminationDetachedWire) { c.ObserverCertificateDigest = "" },
		"node":                         func(c *terminationDetachedWire) { c.NodeUID = "bad" },
		"boot":                         func(c *terminationDetachedWire) { c.NodeBootID = "bad" },
		"action":                       func(c *terminationDetachedWire) { c.Action = "kill" },
		"phase":                        func(c *terminationDetachedWire) { c.Phase = "unknown" },
		"reserved-proof":               func(c *terminationDetachedWire) { c.TerminalProof = []byte("proof") },
		"reserved-cleanup":             func(c *terminationDetachedWire) { c.CleanupDigest = strings.Repeat("c", 64) },
		"terminated-missing-proof":     func(c *terminationDetachedWire) { c.Phase = "terminated" },
		"terminated-big-proof": func(c *terminationDetachedWire) {
			c.Phase = "terminated"
			c.TerminalProof = bytes.Repeat([]byte{1}, 4097)
		},
		"terminated-cleanup": func(c *terminationDetachedWire) {
			c.Phase = "terminated"
			c.TerminalProof = []byte("proof")
			c.CleanupDigest = strings.Repeat("c", 64)
		},
		"cleanup-missing-digest": func(c *terminationDetachedWire) {
			c.Phase = "cleanup-complete"
			c.TerminalProof = []byte("proof")
		},
	}
	for _, field := range []string{"Task", "CleanupIntent", "TaskLink", "Placement", "Control", "Owner", "Fence", "RuntimeIndex", "Quiescence", "QuiescenceReceipt", "QuiescenceReservation", "FinalSync", "Issuer"} {
		changes["required-stamp-"+field] = func(c *terminationDetachedWire) {
			reflect.ValueOf(&c.Authority).Elem().FieldByName(field).Set(reflect.ValueOf(RecordStamp{}))
		}
	}
	for _, field := range []string{"RequestID", "StageID", "AttemptID", "Digest"} {
		changes["quiescence-stage-"+field] = func(c *terminationDetachedWire) {
			reflect.ValueOf(&c.Authority.QuiescenceStage).Elem().FieldByName(field).SetString("")
		}
	}

	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c := terminationDetachedFixture(t)
			change(&c)
			w := terminationMarshal(t, c)
			_, err := DecodeTaskRuntimeTerminationIntent(w)
			require.Error(t, err)

			var intent TaskRuntimeTerminationIntent
			require.NoError(t, json.Unmarshal(w, &intent))
			_, err = EncodeTaskRuntimeTerminationIntent(intent)
			require.Error(t, err)
		})
	}
}

func TestDetachedTerminationVersionOneRejectsDetachedAndMissingLocal(t *testing.T) {
	c := terminationDetachedFixture(t)
	c.Version = 1
	c.Authority.Archive = c.Authority.Task
	c.Authority.LocalComplete = c.Authority.Task
	w := terminationMarshal(t, c)
	_, err := DecodeTaskRuntimeTerminationIntent(w)
	require.Error(t, err)
	var intent TaskRuntimeTerminationIntent
	require.NoError(t, json.Unmarshal(w, &intent))
	_, err = EncodeTaskRuntimeTerminationIntent(intent)
	require.Error(t, err)

	for _, field := range []string{"Archive", "LocalComplete"} {
		legacy := terminationLegacyFixture(t)
		reflect.ValueOf(&legacy.Authority).Elem().FieldByName(field).Set(reflect.ValueOf(RecordStamp{}))
		_, err := DecodeTaskRuntimeTerminationIntent(terminationMarshal(t, legacy))
		require.Error(t, err)
	}
}

func TestDetachedTerminationIntentStrictCanonical(t *testing.T) {
	c := terminationDetachedFixture(t)
	w := terminationMarshal(t, c)
	tuple := terminationMarshal(t, c.DetachedEffects)
	terminationRoundTrip(t, w)
	mutations := map[string][]byte{
		"duplicate-version":     bytes.Replace(w, []byte(`"version":2`), []byte(`"version":2,"version":2`), 1),
		"duplicate-tuple":       bytes.Replace(w, []byte(`"detached_effects":`), []byte(`"detached_effects":null,"detached_effects":`), 1),
		"tuple-null":            bytes.Replace(w, tuple, []byte(`null`), 1),
		"nested-duplicate":      bytes.Replace(w, []byte(`"profile":"detached_enrolled_plain"`), []byte(`"profile":"detached_enrolled_plain","profile":"detached_enrolled_plain"`), 1),
		"nested-unknown":        bytes.Replace(w, []byte(`"profile":"detached_enrolled_plain"`), []byte(`"extra":true,"profile":"detached_enrolled_plain"`), 1),
		"profile-null":          bytes.Replace(w, []byte(`"profile":"detached_enrolled_plain"`), []byte(`"profile":null`), 1),
		"profile-missing":       bytes.Replace(w, []byte(`"profile":"detached_enrolled_plain",`), nil, 1),
		"contract-null":         bytes.Replace(w, []byte(`"contract_digest":"`+strings.Repeat("a", 64)+`"`), []byte(`"contract_digest":null`), 1),
		"settlement-null":       bytes.Replace(w, []byte(`"settlement":{"create_revision":2,"mod_revision":2,"sha256":"`+strings.Repeat("b", 64)+`"}`), []byte(`"settlement":null`), 1),
		"settlement-missing":    bytes.Replace(w, []byte(`,"settlement":{"create_revision":2,"mod_revision":2,"sha256":"`+strings.Repeat("b", 64)+`"}`), nil, 1),
		"zero-stamp-null":       bytes.Replace(w, []byte(`"archive":{"create_revision":0,"mod_revision":0,"sha256":""}`), []byte(`"archive":null`), 1),
		"zero-stamp-incomplete": bytes.Replace(w, []byte(`"local_complete":{"create_revision":0,"mod_revision":0,"sha256":""}`), []byte(`"local_complete":{}`), 1),
		"zero-stamp-missing":    bytes.Replace(w, []byte(`"archive":{"create_revision":0,"mod_revision":0,"sha256":""},`), nil, 1),
		"noncanonical-number":   bytes.Replace(w, []byte(`"version":2`), []byte(`"version":2.0`), 1),
		"escaped-profile":       bytes.Replace(w, []byte(`"profile":"detached_enrolled_plain"`), []byte(`"profile":"\u0064etached_enrolled_plain"`), 1),
		"whitespace":            append([]byte(" "), w...),
		"trailing":              append(bytes.Clone(w), []byte("{}")...),
		"oversize":              bytes.Repeat([]byte(" "), 16385),
	}
	for name, bad := range mutations {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, w, bad, "mutation must change the wire")
			_, err := DecodeTaskRuntimeTerminationIntent(bad)
			require.Error(t, err)
		})
	}
}

func TestDetachedTerminationTicketPinsCompleteIntentDigest(t *testing.T) {
	f := taskCloseSetup(t)
	c := terminationDetachedFixture(t)
	c.Current.IssuerCertificateID = f.issuer.CertificateID()
	c.Current.IssuerCertificateDigest = f.issuer.Digest()
	w := terminationMarshal(t, c)
	terminationRoundTrip(t, w)
	digest := func(w []byte) string {
		sum := sha256.Sum256(w)
		return hex.EncodeToString(sum[:])
	}
	claims := TaskRuntimeTerminationTicketClaims{
		Version: 1, Purpose: "task_runtime_terminate", Current: c.Current, Partition: c.Partition,
		IntentCreateRevision: 13, IntentModRevision: 13, IntentDigest: digest(w),
		EnrollmentDigest: c.EnrollmentDigest, ObserverCertificateDigest: c.ObserverCertificateDigest,
		NodeUID: c.NodeUID, NodeBootID: c.NodeBootID, NotBefore: f.ticket.NotBefore, NotAfter: f.ticket.NotAfter,
	}
	ticket, err := SignTaskRuntimeTerminationTicket(f.issuerKey, f.issuer, claims)
	require.NoError(t, err)
	// Independent signing preserves the existing v1 domain and exact schema.
	require.Equal(t, taskCloseRaw(t, f.issuerKey, claims, "sandbox-task-runtime-termination-ticket:v1\x00"), ticket)
	evidence, err := f.verifier.VerifyTaskRuntimeTerminationTicket(ticket, f.issuerWire, claims, f.now)
	require.NoError(t, err)
	require.Equal(t, claims, evidence.Claims())

	for _, field := range []string{"create_revision", "mod_revision", "sha256", "contract_digest", "local-profile"} {
		t.Run(field, func(t *testing.T) {
			changed := terminationDetachedFixture(t)
			changed.Current = c.Current
			switch field {
			case "create_revision":
				changed.DetachedEffects.Settlement.CreateRevision = 1
			case "mod_revision":
				changed.DetachedEffects.Settlement.ModRevision = 3
			case "sha256":
				changed.DetachedEffects.Settlement.SHA256 = strings.Repeat("d", 64)
			case "contract_digest":
				changed.DetachedEffects.ContractDigest = strings.Repeat("e", 64)
			case "local-profile":
				changed.Version = 1
				changed.DetachedEffects = nil
				changed.Authority.Archive = changed.Authority.Task
				changed.Authority.LocalComplete = changed.Authority.Task
			}

			changedWire := terminationMarshal(t, changed)
			terminationRoundTrip(t, changedWire)
			expected := claims
			expected.IntentDigest = digest(changedWire)
			require.NotEqual(t, claims.IntentDigest, expected.IntentDigest)
			_, err := f.verifier.VerifyTaskRuntimeTerminationTicket(ticket, f.issuerWire, expected, f.now)
			require.Error(t, err)
		})
	}
}

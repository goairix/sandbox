package controlprotocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func nodeTerminationIntentFixture(t *testing.T) TaskRuntimeTerminationIntent {
	f := taskCloseSetup(t)
	c := f.ticket.Context
	c.WorkspaceHash = strings.Repeat("ab", 32)
	s := RecordStamp{CreateRevision: 2, ModRevision: 3, SHA256: strings.Repeat("c", 64)}
	return TaskRuntimeTerminationIntent{
		Version: 1, Phase: "reserved", Current: c, Partition: 0xab, TaskBirth: 4,
		CreationIntentID: "ca1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", MountAttempt: 0,
		EnrollmentDigest: strings.Repeat("d", 64), ObserverCertificateDigest: strings.Repeat("e", 64),
		NodeUID: "da1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", NodeBootID: "ea1c1595-cbfd-4d2e-9ef7-f4dd3c84b172",
		Action:    "kill_pid1_and_observe_cgroup_empty",
		Authority: TerminationAuthorityBindings{Task: s, CleanupIntent: s, TaskLink: s, Placement: s, Control: s, Owner: s, Fence: s, RuntimeIndex: s, Quiescence: s, QuiescenceReceipt: s, QuiescenceReservation: s, FinalSync: s, Issuer: s, Archive: s, LocalComplete: s, QuiescenceStage: TerminationQuiescenceStage{RequestID: "fa1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", StageID: "quiesce_users", AttemptID: "0a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Digest: strings.Repeat("f", 64)}},
	}
}

func TestNodeObserverProtocolIntent(t *testing.T) {
	c := nodeTerminationIntentFixture(t)
	w, err := EncodeTaskRuntimeTerminationIntent(c)
	require.NoError(t, err)
	got, err := DecodeTaskRuntimeTerminationIntent(w)
	require.NoError(t, err)
	require.Equal(t, c, got)
	for name, bad := range map[string][]byte{
		"duplicate":   bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"unknown":     bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"pid":1`), 1),
		"null":        bytes.Replace(w, []byte(`"task_birth":4`), []byte(`"task_birth":null`), 1),
		"missing":     bytes.Replace(w, []byte(`"task_birth":4,`), nil, 1),
		"whitespace":  append([]byte(" "), w...),
		"huge":        bytes.Repeat([]byte(" "), 16385),
		"nested-null": bytes.Replace(w, []byte(`"task":{"create_revision":2,"mod_revision":3,"sha256":"`+strings.Repeat("c", 64)+`"}`), []byte(`"task":null`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeTaskRuntimeTerminationIntent(bad)
			require.Error(t, err)
		})
	}
	for name, change := range map[string]func(*TaskRuntimeTerminationIntent){
		"partition":         func(c *TaskRuntimeTerminationIntent) { c.Partition++ },
		"action":            func(c *TaskRuntimeTerminationIntent) { c.Action = "kill" },
		"phase":             func(c *TaskRuntimeTerminationIntent) { c.Phase = "terminated" },
		"stamp":             func(c *TaskRuntimeTerminationIntent) { c.Authority.Task.ModRevision = 0 },
		"proof-in-reserved": func(c *TaskRuntimeTerminationIntent) { c.TerminalProof = []byte("proof") },
	} {
		t.Run(name, func(t *testing.T) {
			v := c
			change(&v)
			_, err := EncodeTaskRuntimeTerminationIntent(v)
			require.Error(t, err)
			bad, err := json.Marshal(v)
			require.NoError(t, err)
			_, err = DecodeTaskRuntimeTerminationIntent(bad)
			require.Error(t, err)
		})
	}
}

func TestNodeObserverProtocolFixedKeysAndClaim(t *testing.T) {
	c := nodeTerminationIntentFixture(t)
	k, err := TaskRuntimeTerminationKeysFor(c)
	require.NoError(t, err)
	p := c.Current.Namespace + "p/ab/"
	task := p + "tasks/" + c.Current.TaskID
	require.Equal(t, task+"/runtime-termination", k.Intent)
	require.Equal(t, task+"/workspace-sync-local-complete", k.LocalComplete)
	require.Equal(t, p+"sandboxes/"+c.Current.SandboxID+"/operations/", k.OperationsPrefix)
	require.Equal(t, c.Current.Namespace+"indexes/runtime/"+terminationRuntimeIndexDigest(c.Current.Runtime.UID), k.RuntimeIndex)
	require.Equal(t, "3ebf0b7bc368fb1ef70616244fda16e3df82cccd0cb16421a8c10bac9a3c87ff", terminationRuntimeIndexDigest("pod-uid"))
	require.Equal(t, task+"/claim", k.Claim)
	require.Equal(t, task+"/guards/"+c.Current.ClaimID, k.Guard)
	require.Equal(t, p+"cleanup-intents/"+c.Current.TaskID, k.CleanupIntent)
	require.Equal(t, p+"sandboxes/"+c.Current.SandboxID+"/cleanup-task", k.TaskLink)
	require.Equal(t, c.Current.Namespace+"indexes/sandbox/"+c.Current.SandboxID, k.Placement)
	require.Equal(t, p+"controls/"+c.Current.SandboxID, k.Control)
	require.Equal(t, p+"workspaces/"+c.Current.WorkspaceHash+"/owner", k.Owner)
	require.Equal(t, p+"workspaces/"+c.Current.WorkspaceHash+"/fence", k.Fence)
	require.Equal(t, task+"/quiesce-users", k.Quiescence)
	require.Equal(t, task+"/quiesce-users-attempt", k.QuiescenceReservation)
	require.Equal(t, task+"/final-sync-attempt", k.FinalSync)
	require.Equal(t, task+"/workspace-archive-command", k.Archive)
	require.Equal(t, c.Current.Namespace+"command-issuers/"+c.Current.IssuerCertificateID, k.Issuer)
	w, err := EncodeTaskRuntimeTerminationClaim(c.Current, c.Partition)
	require.NoError(t, err)
	var v struct {
		Version uint32 `json:"version"`
		Task    struct {
			Namespace    string `json:"namespace"`
			RestoreEpoch string `json:"restore_epoch"`
			TaskID       string `json:"task_id"`
			SandboxID    string `json:"sandbox_id"`
			Partition    uint8  `json:"partition"`
		} `json:"task"`
		ClaimID  string `json:"claim_id"`
		WorkerID string `json:"worker_id"`
		LeaseID  int64  `json:"lease_id"`
	}
	require.NoError(t, json.Unmarshal(w, &v))
	require.Equal(t, c.Current.ClaimID, v.ClaimID)
	require.Equal(t, c.Current.LeaseID, v.LeaseID)
	require.Equal(t, c.Partition, v.Task.Partition)
}

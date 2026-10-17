//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// This private test manifest carries bytes, never live capabilities or a
// callback. Its parent owns the genuine cell and signing root. RestoreEpoch
// must be reconstructed from Binding because Identity deliberately omits it
// from its persisted JSON representation.
type cleanupCapacityChildInput struct {
	Binding       p.TrustBinding
	ConfigBinding p.TrustBinding
	Identity      Identity
	Root          ed25519.PrivateKey
	RootPublic    ed25519.PublicKey
	PhysicalTask  TaskReference
	Contract      string
	Directory     string
	Nonce         string
}

// ACKs are bounded observations of this one fixed 33-Task experiment. They
// cannot be passed to any admission, Run, physical effect or settlement API.
type cleanupCapacityChildACK struct {
	Nonce, Phase           string
	Slot                   uint32
	Rows, Turns, Conflicts int
}

func cleanupCapacityChildRead(t *testing.T, path string) cleanupCapacityChildInput {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	owner, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Zero(t, owner.Uid)
	require.LessOrEqual(t, info.Size(), int64(65536))
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	wire, err := io.ReadAll(io.LimitReader(file, 65537))
	require.NoError(t, err)
	defer clear(wire)
	require.LessOrEqual(t, len(wire), 65536)
	var input cleanupCapacityChildInput
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&input))
	require.Equal(t, io.EOF, decoder.Decode(new(any)))
	require.Len(t, input.Root, ed25519.PrivateKeySize)
	require.Len(t, input.RootPublic, ed25519.PublicKeySize)
	require.True(t, bytes.Equal(input.Root.Public().(ed25519.PublicKey), input.RootPublic))
	require.Equal(t, input.Binding, input.ConfigBinding)
	require.Equal(t, "kubernetes", input.Binding.Target)
	require.Equal(t, input.Binding.AuthorityID, input.Identity.RuntimeID)
	input.Identity.RestoreEpoch = input.Binding.RestoreEpoch
	require.NoError(t, input.PhysicalTask.Validate())
	require.Equal(t, input.Binding.Namespace, input.PhysicalTask.Namespace)
	require.Equal(t, input.Binding.RestoreEpoch, input.PhysicalTask.RestoreEpoch)
	require.NotEqual(t, uint8(0xaa), input.PhysicalTask.Partition)
	require.Len(t, input.Nonce, 36)
	require.True(t, validHexDigest(input.Contract))
	return input
}

// Only the three fixed bytes 1=CHECK33, 2=RETRY33, 3=STOP are accepted in that
// order. One reader and one buffered command bound communication independently
// of Task count. Closing the owned pipe unblocks and joins that reader before
// session cleanup; the sole foreground goroutine owns every native turn/renew.
func TestCleanupPersistentCapacityChild(t *testing.T) {
	path := os.Getenv("CLEANUP_CAPACITY_CHILD_INPUT")
	if path == "" {
		return
	}
	require.Zero(t, os.Geteuid())
	input := cleanupCapacityChildRead(t, path)
	defer clear(input.Root)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	require.NoError(t, syscall.SetNonblock(3, true))
	commands := os.NewFile(3, "capacity-commands")
	replies := os.NewFile(4, "capacity-replies")
	require.NotNil(t, commands)
	require.NotNil(t, replies)
	defer replies.Close()
	queue := make(chan byte, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var command [1]byte
			_, err := io.ReadFull(commands, command[:])
			if err != nil {
				command[0] = 0
			}
			select {
			case queue <- command[0]:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var stopReader sync.Once
	joinReader := func() {
		stopReader.Do(func() {
			cancel()
			_ = commands.Close()
			<-readerDone
		})
	}
	defer joinReader()

	ca, err := os.ReadFile(input.Directory + "/ca.pem")
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	certificate, err := tls.LoadX509KeyPair(input.Directory+"/server.pem", input.Directory+"/server-key.pem")
	require.NoError(t, err)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{certificate}}
	// The input is root-only, but every subsequent metadata call runs as the
	// ordinary zero-cap UID. This role never bootstraps a physical Kernel.
	for capability := uintptr(0); capability <= 40; capability++ {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0, 0, 0, 0)
		require.Zero(t, errno)
	}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_SETGROUPS, 0, 0, 0)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESGID, 1000, 1000, 1000)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESUID, 1000, 1000, 1000)
	require.Zero(t, errno)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: 2 * time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer raw.Close()
	namespace, err := NewNamespace(input.Identity.Prefix, input.Identity.AuthorityID, input.Identity.Cell)
	require.NoError(t, err)
	require.Equal(t, input.Binding.Namespace, namespace.Root())
	options := Options{
		Endpoints: raw.Endpoints(), Namespace: namespace, Identity: input.Identity, TLS: tlsConfig,
		RequestTimeout: 2 * time.Second, Clock: goodAuthorityClock(),
		PublicationTrust: &RuntimePublicationTrust{AuthorityID: input.Binding.AuthorityID, Target: input.Binding.Target, Roots: []ed25519.PublicKey{input.RootPublic}},
		WorkerControl:    &WorkerControlOptions{SessionTTL: 60 * time.Second},
	}
	b, err := New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	// This separate Backend only authenticates the genuine physical release
	// history. Ordinary metadata publication above must retain enrolled=nil.
	proofOptions := options
	proofOptions.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: input.Contract, MaxActive: 32}
	proofReader, err := New(ctx, proofOptions)
	require.NoError(t, err)
	defer proofReader.Close()
	session, err := b.OpenWorkerSession(ctx, "capacity-metadata-child")
	require.NoError(t, err)
	part, err := b.ClaimWorkerPartition(ctx, session, 0xaa)
	require.NoError(t, err)
	peer := &cleanupCapacityPeer{b: b, session: session, part: part}
	for slot := 32; slot < 100; slot++ {
		share, err := b.AcquireWorkerBudget(ctx, session, WorkerBudgetClaim, uint16(slot))
		require.NoError(t, err)
		peer.shares = append(peer.shares, share)
	}
	peer.observeNative(false)
	var closeSession sync.Once
	cleanup := func() {
		closeSession.Do(func() {
			joinReader()
			cleanupCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			require.NoError(t, b.ReleaseWorkerSession(cleanupCtx, session))
			require.NoError(t, b.Close())
			require.NoError(t, proofReader.Close())
		})
	}
	defer cleanup()

	var tasks [32]*cleanupCapacityTaskState
	ordinal := 0
	for candidate := 0; candidate < 65536 && ordinal < len(tasks); candidate++ {
		workspace, err := NewWorkspaceIdentity("s3", input.Identity.StorageID, "bucket", fmt.Sprintf("capacity/%d/", candidate))
		require.NoError(t, err)
		if workspace.Partition() != 0xaa {
			continue
		}
		task := &cleanupCapacityTaskState{ref: cleanupCapacityTask(t, ctx, b, input.Root, workspace, ordinal)}
		require.NoError(t, peer.prepare(ctx, task))
		if ordinal < 31 {
			require.NoError(t, peer.occupy(ctx, task, false))
			require.Nil(t, task.occupied.record.Run)
			require.Equal(t, "reserved", task.occupied.record.Confirmed)
		}
		tasks[ordinal] = task
		ordinal++
	}
	require.Equal(t, len(tasks), ordinal)
	extra := tasks[31]
	metadata := cleanupCapacityRows(t, ctx, raw, namespace.Root())
	require.Len(t, metadata, 63, "31 real S/E plus only the original selecting E33")
	delete(metadata, extra.selected.execution.key)
	require.Len(t, metadata, 62)
	stable := func(rows map[string]*mvccpb.KeyValue) {
		for key, before := range metadata {
			require.Equal(t, before, rows[key], "31 metadata occupations remain exact: %s", key)
		}
	}
	ack := func(phase string, slot uint32) {
		observation, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		wire, err := json.Marshal(cleanupCapacityChildACK{Nonce: input.Nonce, Phase: phase, Slot: slot, Rows: len(cleanupCapacityRows(t, observation, raw, namespace.Root())), Turns: peer.turns, Conflicts: peer.conflicts})
		require.NoError(t, err)
		require.LessOrEqual(t, len(wire)+1, 4096)
		_, err = replies.Write(append(wire, '\n'))
		require.NoError(t, err)
	}
	ack("READY31", 0)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	var full map[string]*mvccpb.KeyValue
	var physical cleanupExecutionRecord
	physicalKey, err := namespace.cleanupExecutionKey(input.PhysicalTask)
	require.NoError(t, err)
	for expected := byte(1); expected <= 3; {
		select {
		case <-ctx.Done():
			t.Fatal("metadata child deadline before fixed protocol completed")
		case <-ticker.C:
			require.NoError(t, b.RenewWorkerSession(ctx, session))
		case command := <-queue:
			require.Equal(t, expected, command, "fixed CHECK33/RETRY33/STOP order; EOF is failure")
			switch command {
			case 1:
				full = cleanupCapacityRows(t, ctx, raw, namespace.Root())
				require.Len(t, full, 65)
				stable(full)
				kv := full[physicalKey]
				require.NotNil(t, kv)
				require.NoError(t, decodeCleanupExecution(kv, kv.ModRevision, input.PhysicalTask, &physical))
				require.Equal(t, "occupied", physical.Phase)
				require.Equal(t, "archive-bound", physical.Confirmed)
				require.NotNil(t, physical.Run)
				require.True(t, physical.Run.Joined)
				require.NotNil(t, physical.Archive)
				delete(full, extra.selected.execution.key)
				require.Len(t, full, 64)
				before := peer.conflicts
				require.NoError(t, peer.occupy(ctx, extra, true))
				require.Equal(t, 32, peer.conflicts-before)
				require.Nil(t, extra.occupied)
				after := cleanupCapacityRows(t, ctx, raw, namespace.Root())
				require.Len(t, after, 65)
				for key, before := range full {
					require.Equal(t, before, after[key], "natural recovery raced CHECK33: %s", key)
				}
				require.True(t, extra.selected.execution.matches(after[extra.selected.execution.key]))
				ack("CHECKED33", physical.Slot)
			case 2:
				before := cleanupCapacityRows(t, ctx, raw, namespace.Root())
				stable(before)
				kv := before[physicalKey]
				require.NotNil(t, kv)
				var settled cleanupExecutionRecord
				require.NoError(t, decodeCleanupExecution(kv, kv.ModRevision, input.PhysicalTask, &settled))
				require.Equal(t, "settled", settled.Phase)
				require.Equal(t, physical.Incarnation, settled.Incarnation)
				require.NotNil(t, settled.Run)
				require.True(t, settled.Run.Joined)
				require.NotNil(t, settled.Release)
				proof, err := proofReader.readLocalOwnerReleaseProof(ctx, input.PhysicalTask)
				require.NoError(t, err)
				require.NotNil(t, proof)
				require.Equal(t, physical.TaskBirth, proof.record.TaskBirth)
				require.Equal(t, settled.Release.CreateRevision, proof.kv.CreateRevision)
				require.Equal(t, settled.Release.Digest, workspaceSyncIntentDigest(string(proof.kv.Value)))
				slot, err := namespace.cleanupExecutionSlotKey(physical.Slot)
				require.NoError(t, err)
				require.Nil(t, before[slot], "only the genuine physical settlement may free this S")
				require.NoError(t, peer.occupy(ctx, extra, false))
				require.Equal(t, physical.Slot, extra.occupied.record.Slot)
				require.Nil(t, extra.occupied.record.Run)
				stable(cleanupCapacityRows(t, ctx, raw, namespace.Root()))
				ack("RETRIED33", physical.Slot)
			case 3:
				stable(cleanupCapacityRows(t, ctx, raw, namespace.Root()))
				require.Zero(t, peer.claimPuts.Load(), "metadata occupations never obtain physical TaskClaims")
				cleanup()
				ack("STOPPED", physical.Slot)
				return
			}
			expected++
		}
	}
}

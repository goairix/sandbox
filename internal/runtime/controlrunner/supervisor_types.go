// Package controlrunner owns the actual PID1 and fixed trusted monitor processes.
// Diagnostics and persisted history never reconstruct execution authority.
package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

var (
	ErrInvalidConfiguration = errors.New("invalid supervisor configuration")
	ErrUnavailable          = errors.New("supervisor unavailable")
	ErrAdmissionClosed      = errors.New("supervisor admission closed")
	ErrExecutionUnknown     = errors.New("execution outcome unknown")
)

type SupervisorOptions struct {
	Kernel           *launcher.KernelBoundary
	UID, GID         uint32
	NetworkAllowed   bool
	ContractDigest   string
	Verifier         *controlprotocol.ManagementVerifier
	Clock            controlprotocol.AuthorityClock
	JournalDirectory string
	JournalMaxBytes  int64
	Executable       string
	MaxActive        uint32
}
type Supervisor struct {
	self                      *Supervisor
	pid                       int
	mu                        sync.Mutex
	bootstrapMu               sync.Mutex
	transport                 transportLifetime
	options                   SupervisorOptions
	birth                     controlprotocol.BirthContext
	key                       ed25519.PrivateKey
	executable                os.FileInfo
	activation                *controlprotocol.TargetActivationEvidence
	journal                   *controltarget.Journal
	credential                *controlprotocol.ManagementTLSCredential
	tlsConfig                 *tls.Config
	active                    map[string]*Execution
	admission, closed, failed bool
	failurePending            atomic.Bool
	usersClosed               atomic.Bool
	quiescence                *userQuiescenceAttempt
	deadlineMu                sync.Mutex
	isolationDeadline         *isolationDeadline
	failureOnce               sync.Once
	activeView                atomic.Pointer[[]*Execution]
	isolationPublished        atomic.Bool
}

// authenticatedStart is constructed only by the package's authenticated TLS
// delivery handler (Task4). There is no exported request-to-execution entry.
type authenticatedStart struct {
	supervisor *Supervisor
	descriptor controlprotocol.ExecutionDescriptor
	evidence   controlprotocol.ExecStartEvidence
}

// Execution is an original in-memory registration and bounded stream/result
// owner. It has no public constructor and cannot be reconstructed from history.
type Execution struct {
	self                                   *Execution
	supervisor                             *Supervisor
	mu                                     sync.Mutex
	accepted                               *controltarget.AcceptedExecution
	descriptor                             controlprotocol.ExecutionDescriptor
	commandID                              string // immutable original registration key, published before active registry
	record                                 controltarget.ExecJournalRecord
	acceptedReceipt, resultReceipt         []byte
	state                                  string
	rootPID                                int
	stats                                  monitorReady
	monitorPID                             int
	control                                *os.File
	request                                *os.File
	result                                 *os.File
	cancel                                 context.CancelFunc
	ownerDone, waitDone                    chan struct{}
	waitErr                                error
	output                                 chan streamFrame
	ack                                    chan renewAck
	authorityDeadlineNS, commandDeadlineNS int64
	terminalSeen                           atomic.Bool
	sequence                               uint64
	runContext                             context.Context
	watchdog                               *time.Timer
	watchdogDone                           chan struct{}
	diagnostic                             atomic.Pointer[isolationExecution]
	receiptIssued                          atomic.Bool
	watchdogDeadline                       atomic.Int64
	startCommitted                         bool
	closeRequested                         bool
}
type streamFrame struct {
	kind byte
	data []byte
}
type renewAck struct {
	sequence uint64
	deadline int64
}

//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"golang.org/x/sys/unix"
)

type enrollmentRecord struct {
	Version             uint32                  `json:"version"`
	Request             p.NodeEnrollmentRequest `json:"request"`
	ObserverCertificate []byte                  `json:"observer_certificate"`
	Kernel              processFacts            `json:"kernel"`
	Provider            providerObservation     `json:"provider"`
	ObservedAt          time.Time               `json:"observed_at"`
}

func canonicalObserver(wire []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, wire) {
		return fmt.Errorf("noncanonical observer journal record")
	}
	return nil
}

func (o *Observer) observe(ctx context.Context) (p.ClockObservation, error) {
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	observation, err := o.options.Clock.Observe(bounded)
	elapsed := time.Since(started)
	if err != nil || bounded.Err() != nil || observation.UTC.IsZero() || observation.UTC.Location() != time.UTC || observation.Uncertainty < 0 || observation.Uncertainty > time.Second || elapsed > time.Second-observation.Uncertainty {
		return p.ClockObservation{}, fmt.Errorf("observer controlled clock unavailable")
	}
	observation.UTC = observation.UTC.Add(elapsed)
	observation.Uncertainty += elapsed
	return observation, nil
}

func validateProcessBoundary(anchor *processAnchor) error {
	status, err := readProcAt(anchor.proc, "status", 65536)
	if err != nil {
		return err
	}
	if err := validateProcessStatus(status); err != nil {
		return err
	}
	var own unix.Stat_t
	if err := unix.Stat("/proc/self/ns/mnt", &own); err != nil || (descriptorIdentity{Device: uint64(own.Dev), Inode: own.Ino}) == anchor.facts.MountNS {
		return fmt.Errorf("peer shares observer mount namespace")
	}
	mounts, err := readProcAt(anchor.proc, "mountinfo", 65536)
	if err != nil {
		return err
	}
	found := false
	for _, line := range strings.Split(string(mounts), "\n") {
		parts := strings.Fields(line)
		for i, field := range parts {
			if field != "-" || i+1 >= len(parts) || (parts[i+1] != "cgroup" && parts[i+1] != "cgroup2") {
				continue
			}
			if len(parts) < 6 || !strings.Contains(","+parts[5]+",", ",ro,") || parts[i+1] != "cgroup2" {
				return fmt.Errorf("peer has writable/ambiguous cgroup view")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("peer cgroup confinement view unavailable")
	}
	return nil
}

func validateProcessStatus(status []byte) error {
	fields := make(map[string][]string)
	for _, line := range strings.Split(string(status), "\n") {
		parts := strings.Fields(line)
		if len(parts) > 1 {
			fields[strings.TrimSuffix(parts[0], ":")] = parts[1:]
		}
	}
	if len(fields["NoNewPrivs"]) != 1 || fields["NoNewPrivs"][0] != "1" || len(fields["Seccomp"]) != 1 || fields["Seccomp"][0] != "2" {
		return fmt.Errorf("enrollment requires actual no_new_privs and seccomp")
	}
	for _, name := range []string{"CapEff", "CapPrm", "CapBnd", "CapAmb", "CapInh"} {
		parts := fields[name]
		if len(parts) != 1 {
			return fmt.Errorf("peer capability identity unavailable")
		}
		value, err := strconv.ParseUint(parts[0], 16, 64)
		want := uint64(1<<unix.CAP_KILL | 1<<unix.CAP_SETUID | 1<<unix.CAP_SETGID)
		if name == "CapAmb" || name == "CapBnd" {
			want = 0
		}
		if err != nil || value != want {
			return fmt.Errorf("peer can escape protected runtime boundary")
		}
	}
	return nil
}

func (o *Observer) receipt(record enrollmentRecord, wire []byte) (p.NodeEnrollmentReceipt, error) {
	a, err := o.verifier.VerifyNodeEnrollmentRequest(record.Request, record.ObservedAt)
	if err != nil {
		return p.NodeEnrollmentReceipt{}, err
	}
	identity := a.Identity()
	return p.SignNodeEnrollmentReceipt(o.options.PrivateKey, o.identity, p.NodeEnrollmentReceiptClaims{Version: 1, Purpose: "runtime_node_enrolled", EnrollmentDigest: digest(wire), ObserverCertificateDigest: digest(record.ObserverCertificate), NodeUID: record.Provider.NodeUID, NodeBootID: record.Provider.NodeBootID, ActivationDigest: a.Digest(), Runtime: identity.Runtime, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, CreationIntentID: record.Provider.CreationIntentID, ObservedAt: record.ObservedAt})
}

func (o *Observer) restoreSlots() error {
	for slot, record := range o.journal.records {
		if record.Version == 0 {
			continue
		}
		var registration enrollmentRecord
		if canonicalObserver(record.Enrollment, &registration) != nil || registration.Version != 1 || len(record.Enrollment) > 8192 || registration.Provider.NodeUID != o.options.NodeUID || registration.Provider.NodeBootID != o.identity.NodeBootID() {
			return fmt.Errorf("observer journal registration/node binding corrupt")
		}
		if _, err := o.verifier.VerifyNodeEnrollmentRequest(registration.Request, registration.ObservedAt); err != nil {
			return err
		}
		if _, err := o.verifier.VerifyNodeObserverCertificate(registration.ObserverCertificate, registration.Provider.NodeUID, registration.Provider.NodeBootID, registration.ObservedAt); err != nil {
			return err
		}
		o.slots[slot] = observerSlot{registration: registration, wire: bytes.Clone(record.Enrollment)}
		if record.State != "enrolled" {
			query, err := retainedQuery(record, digest(record.Enrollment))
			if err != nil {
				return err
			}
			if record.State == "terminal" {
				if _, err := o.verifier.VerifyNodeTerminationProof(record.Terminal, registration.ObserverCertificate, query); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (o *Observer) handleEnrollment(ctx context.Context, conn *net.UnixConn) error {
	pid, err := kernelPeer(conn)
	if err != nil {
		return err
	}
	wire, err := t.ReadNodeObserverFrame(conn)
	if err != nil {
		return err
	}
	request, err := p.DecodeNodeEnrollmentRequest(wire)
	if err != nil {
		return err
	}
	now, err := o.observe(ctx)
	if err != nil {
		return err
	}
	activation, err := o.verifier.VerifyNodeEnrollmentRequest(request, now.UTC)
	if err != nil {
		return err
	}
	provider := providerRegistration{Identity: activation.Identity(), Birth: activation.Birth()}
	observed, err := o.provider.observe(ctx, provider, false)
	if err != nil {
		return err
	}
	provider.CreationIntentID = observed.CreationIntentID
	anchor, err := openProcess(pid)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.close() }()
	if err := validateProcessBoundary(anchor); err != nil {
		return err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	challenge := p.NodeEnrollmentChallenge{Version: 1, Purpose: "runtime_node_enrollment_challenge", ObserverCertificate: bytes.Clone(o.options.Certificate), Nonce: hex.EncodeToString(nonce[:]), RequestDigest: digest(wire), Runtime: activation.Identity().Runtime, NodeUID: o.options.NodeUID, NodeBootID: o.identity.NodeBootID()}
	challengeWire, err := p.EncodeNodeEnrollmentChallenge(challenge)
	if err != nil {
		return err
	}
	if err := t.WriteNodeObserverFrame(conn, challengeWire); err != nil {
		return err
	}
	responseWire, err := t.ReadNodeObserverFrame(conn)
	if err != nil {
		return err
	}
	response, err := p.DecodeNodeEnrollmentResponse(responseWire)
	if err != nil {
		return err
	}
	now, err = o.observe(ctx)
	if err != nil {
		return err
	}
	runtime, err := o.verifier.VerifyRuntimeIdentityCertificate(request.RuntimeCertificate, activation.Identity(), now.UTC)
	if err != nil {
		return err
	}
	if err := p.VerifyNodeEnrollmentResponse(challenge, response, runtime); err != nil {
		return err
	}
	if peer, err := kernelPeer(conn); err != nil || peer != pid {
		return fmt.Errorf("enrollment kernel peer changed")
	}
	if err := anchor.revalidate(); err != nil {
		return err
	}
	if err := validateProcessBoundary(anchor); err != nil {
		return err
	}
	observed, err = o.provider.observe(ctx, provider, false)
	if err != nil {
		return err
	}
	if boot, err := bootID(); err != nil || boot != o.identity.NodeBootID() {
		return fmt.Errorf("enrollment host boot changed")
	}
	now, err = o.observe(ctx)
	if err != nil {
		return err
	}
	registration := enrollmentRecord{Version: 1, Request: request, ObserverCertificate: bytes.Clone(o.options.Certificate), Kernel: anchor.facts, Provider: observed, ObservedAt: now.UTC}
	registrationWire, err := json.Marshal(registration)
	if err != nil || len(registrationWire) > 8192 {
		return fmt.Errorf("observer enrollment record exceeds fixed slot bound")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopping || ctx.Err() != nil {
		return fmt.Errorf("observer enrollment stopped")
	}
	for i := range o.slots {
		existing := &o.slots[i]
		if existing.registration.Version == 0 {
			continue
		}
		old, err := o.verifier.VerifyNodeEnrollmentRequest(existing.registration.Request, existing.registration.ObservedAt)
		if err != nil {
			return err
		}
		if old.Identity().Runtime != activation.Identity().Runtime {
			continue
		}
		oldRequest, _ := p.EncodeNodeEnrollmentRequest(existing.registration.Request)
		if !bytes.Equal(oldRequest, wire) || existing.registration.Kernel != anchor.facts || existing.registration.Provider.CreationIntentID != observed.CreationIntentID || !bytes.Equal(existing.registration.ObserverCertificate, o.options.Certificate) {
			return fmt.Errorf("observer enrollment incarnation substitution refused")
		}
		if existing.anchor == nil {
			existing.anchor, anchor = anchor, nil
		}
		receipt, err := o.receipt(existing.registration, existing.wire)
		if err != nil {
			return err
		}
		return t.WriteNodeObserverFrame(conn, receipt.Wire())
	}
	// Preflight the full allowed 16 KiB intent plus fixed maximum ticket/proof
	// overhead before consuming an enrollment. Actual acceptance rechecks the
	// complete canonical record, with no truncation before any signal.
	if len(registrationWire)+16384+4096+4096+512 > maxJournalSlotBytes {
		return fmt.Errorf("observer enrollment cannot fit full termination state")
	}
	slot, err := o.journal.reserve(journalRecord{Version: 1, State: "enrolled", Enrollment: registrationWire})
	if err != nil {
		return err
	}
	o.slots[slot] = observerSlot{registration: registration, wire: bytes.Clone(registrationWire), anchor: anchor}
	anchor = nil
	receipt, err := o.receipt(registration, registrationWire)
	if err != nil {
		return err
	}
	return t.WriteNodeObserverFrame(conn, receipt.Wire())
}

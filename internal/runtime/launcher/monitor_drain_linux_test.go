//go:build linux && (amd64 || arm64)

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const drainStatusFixture = "Name:\ttest\nState:\tZ (zombie)\nTgid:\t42\nPid:\t42\nPPid:\t9\nUid:\t1000\t1000\t1000\t1000\nGid:\t1001\t1001\t1001\t1001\nThreads:\t0\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapBnd:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\nSeccomp:\t2\nSeccomp_filters:\t2\n"

func TestMonitorDrainStatus(t *testing.T) {
	m := &MonitorBoundary{pid: 9, uid: 1000, gid: 1001, installedFilters: 2, cgroup: []byte("0::/\n")}
	s, err := parseDrainStatus([]byte(drainStatusFixture), 42)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.checkDrainChild(s, []byte("0::/\n")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Pid", "Tgid", "PPid", "Uid", "Gid", "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp", "Seccomp_filters"} {
		start := strings.Index(drainStatusFixture, key+":")
		end := start + strings.IndexByte(drainStatusFixture[start:], '\n') + 1
		line := drainStatusFixture[start:end]
		for name, value := range map[string]string{"missing": strings.Replace(drainStatusFixture, line, "", 1), "duplicate": drainStatusFixture + line, "signed": strings.Replace(drainStatusFixture, line, key+": +1\n", 1), "overflow": strings.Replace(drainStatusFixture, line, key+": 18446744073709551616\n", 1)} {
			t.Run(key+"_"+name, func(t *testing.T) {
				if _, e := parseDrainStatus([]byte(value), 42); e == nil {
					t.Fatal("accepted malformed child")
				}
			})
		}
	}
	for _, pair := range [][2]string{{"Pid:\t42", "Pid:\t43"}, {"Tgid:\t42", "Tgid:\t43"}, {"PPid:\t9", "PPid:\t0"}, {"Uid:\t1000\t1000\t1000\t1000", "Uid: 1000 0 1000 1000"}, {"Gid:\t1001\t1001\t1001\t1001", "Gid: 1001 1001 0 1001"}, {"CapEff:\t0000000000000000", "CapEff: 1"}, {"NoNewPrivs:\t1", "NoNewPrivs: 0"}, {"Seccomp:\t2", "Seccomp: 0"}, {"Seccomp_filters:\t2", "Seccomp_filters: 1"}} {
		s, e := parseDrainStatus([]byte(strings.Replace(drainStatusFixture, pair[0], pair[1], 1)), 42)
		if e == nil {
			e = m.checkDrainChild(s, []byte("0::/\n"))
		}
		if e == nil {
			t.Errorf("accepted %v", pair)
		}
	}
	for _, value := range []string{strings.Repeat("x", 65537), strings.ReplaceAll(drainStatusFixture, "\n", "\r\n"), strings.TrimSuffix(drainStatusFixture, "\n")} {
		if _, e := parseDrainStatus([]byte(value), 42); e == nil {
			t.Fatal("accepted malformed bytes")
		}
	}
	if e := m.checkDrainChild(s, []byte("0::/other\n")); e == nil {
		t.Fatal("cgroup mismatch accepted")
	}
}

func drainTestHandle() *MonitorBoundary {
	m := &MonitorBoundary{pid: os.Getpid(), confined: true}
	m.self = m
	return m
}
func drainTestContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Second)
}
func TestMonitorDrainOwnerAndContext(t *testing.T) {
	ctx, cancel := drainTestContext()
	defer cancel()
	for _, m := range []*MonitorBoundary{nil, {}, {pid: os.Getpid() + 1}} {
		if o, e := m.Drain(ctx); e == nil || o.MonitorPID != 0 || o.Reaped != nil {
			t.Fatalf("invalid handle: %+v %v", o, e)
		}
	}
	m := drainTestHandle()
	calls := drainCalls{validate: func() error { t.Fatal("validation reached"); return nil }}
	for _, c := range []context.Context{nil, context.Background()} {
		if _, e := m.drainWith(c, calls); e == nil || m.poisoned || m.drainStarted {
			t.Fatal("invalid context mutated")
		}
	}
	m.mu.Lock()
	_, e := m.drainWith(ctx, calls)
	m.mu.Unlock()
	if !errors.Is(e, ErrKernelUnavailable) || m.poisoned {
		t.Fatal("concurrent owner")
	}
}

func TestMonitorDrainLoop(t *testing.T) {
	for _, scenario := range []string{"fresh", "wait error", "scan error", "validation error", "final validation error", "malformed wait", "overflow", "eintr cancel", "vanished", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := drainTestContext()
			defer cancel()
			m := drainTestHandle()
			waits, scans, validations := 0, 0, 0
			if scenario == "overflow" {
				m.reaped = make([]ProcessExit, 4096)
			}
			calls := drainCalls{
				validate: func() error {
					validations++
					if scenario == "validation error" || (scenario == "final validation error" && validations == 2) {
						return unix.EIO
					}
					return nil
				},
				wait: func(status *unix.WaitStatus) (int, error) {
					waits++
					switch scenario {
					case "wait error":
						return -1, unix.EPERM
					case "malformed wait":
						*status = unix.WaitStatus(0x7f)
						return 42, nil
					case "eintr cancel":
						cancel()
						return -1, unix.EINTR
					case "overflow":
						*status = unix.WaitStatus(9)
						return 42, nil
					case "scan error", "deadline":
						return 0, nil
					case "vanished":
						if waits == 1 {
							return 0, nil
						}
					}
					if waits == 1 {
						*status = unix.WaitStatus(7 << 8)
						return 42, nil
					}
					return -1, unix.ECHILD
				},
				scan: func(context.Context) error {
					scans++
					if scenario == "scan error" {
						return unix.EIO
					}
					if scenario == "deadline" {
						cancel()
					}
					if scenario == "vanished" {
						return unix.ESRCH
					}
					return nil
				},
			}
			o, e := m.drainWith(ctx, calls)
			good := scenario == "fresh" || scenario == "vanished"
			if good {
				if e != nil || o.MonitorPID != os.Getpid() || m.poisoned || validations != 2 {
					t.Fatalf("success: %+v %v v=%d", o, e, validations)
				}
				if scenario == "fresh" {
					if len(o.Reaped) != 1 || o.Reaped[0].PID != 42 || o.Reaped[0].WaitStatus != 7<<8 {
						t.Fatal(o)
					}
					o.Reaped[0].PID = 99
					if m.reaped[0].PID != 42 {
						t.Fatal("not copied")
					}
				}
				before := waits
				if _, e = m.drainWith(ctx, calls); e != nil || waits <= before {
					t.Fatal("cached success")
				}
			} else {
				if e == nil || o.MonitorPID != 0 || o.Reaped != nil || !m.poisoned {
					t.Fatalf("failure: %+v %v poison=%t", o, e, m.poisoned)
				}
				before := waits
				if _, e = m.drainWith(ctx, calls); e == nil || waits != before {
					t.Fatal("poison ignored")
				}
			}
			if scenario == "vanished" && scans != 1 {
				t.Fatal("vanished did not rescan/reap")
			}
		})
	}
}

func TestMonitorDrainProcessLimit(t *testing.T) {
	count := 0
	for i := 1; i <= 4096; i++ {
		if _, e := drainProcessEntry(fmt.Sprint(i), &count); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := drainProcessEntry("4097", &count); !errors.Is(e, ErrUnsafeKernel) {
		t.Fatal("process bound", e)
	}
}

// A missing policy field on another monitor's child must not block selecting
// only our own children; malformed identity still fails closed.
func TestMonitorDrainIdentitySelection(t *testing.T) {
	data := []byte("Pid: 42\nTgid: 42\nPPid: 8\nCapEff: invalid\n")
	s, err := parseDrainFields(data, 42, true)
	if err != nil || s.ppid != 8 {
		t.Fatalf("unrelated identity: %+v %v", s, err)
	}
	if _, err = parseDrainStatus(data, 42); err == nil {
		t.Fatal("direct child missing policy accepted")
	}
}

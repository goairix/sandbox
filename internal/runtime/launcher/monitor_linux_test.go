//go:build linux && (amd64 || arm64)

package launcher

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMonitorInvalidHandles(t *testing.T) {
	// A regression consuming the global attempt for an invalid role fails this test.
	for _, k := range []*KernelBoundary{nil, {}, {role: rolePID1, pid: os.Getpid()}, {role: roleMonitor, pid: os.Getpid() + 1}} {
		if m, err := ConfineMonitor(k, 1000, 1000); m != nil || err == nil {
			t.Fatalf("invalid kernel: %v %v", m, err)
		}
	}
	if monitorInitialization.attempted {
		t.Fatal("invalid handles consumed attempt")
	}
	for _, m := range []*MonitorBoundary{nil, {}, {pid: os.Getpid() + 1, confined: true}} {
		if err := m.ValidateCurrent(); err == nil {
			t.Fatal("invalid monitor accepted")
		}
	}
}

func TestMonitorSeccompStatus(t *testing.T) {
	for _, s := range []seccompState{{0, 0}, {2, 1}, {2, 4095}, {2, 4096}} {
		text := fmt.Sprintf("Name:\ttest\nSeccomp:\t%d\nSeccomp_filters:\t%d\n", s.mode, s.filters)
		got, err := parseSeccompStatus([]byte(text))
		if err != nil || got != s {
			t.Fatalf("valid %q: %+v %v", text, got, err)
		}
	}
	for _, text := range []string{"", "Seccomp: 2\n", "Seccomp_filters: 1\n", "Seccomp: 2\nSeccomp: 2\nSeccomp_filters: 1\n", "Seccomp: 2\nSeccomp_filters: 1\nSeccomp_filters: 1\n", "Seccomp: 1\nSeccomp_filters: 0\n", "Seccomp: 3\nSeccomp_filters: 1\n", "Seccomp: 0\nSeccomp_filters: 1\n", "Seccomp: 2\nSeccomp_filters: 0\n", "Seccomp: -1\nSeccomp_filters: 1\n", "Seccomp: +2\nSeccomp_filters: 1\n", "Seccomp: 2 2\nSeccomp_filters: 1\n", "Seccomp: 2\nSeccomp_filters: 4294967296\n", strings.Repeat("x", maxStatusBytes+1)} {
		if _, err := parseSeccompStatus([]byte(text)); err == nil {
			t.Errorf("accepted malformed status %q", text[:min(len(text), 100)])
		}
	}
	if _, err := parseSeccompStatus([]byte("Seccomp: 2\n")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("missing facility: %v", err)
	}
}

func TestMonitorSeccompCounts(t *testing.T) {
	for _, tc := range []struct {
		states []seccompState
		floor  uint32
		exact  bool
		want   uint32
		bad    bool
	}{
		{[]seccompState{{0, 0}, {0, 0}}, 0, true, 0, false},
		{[]seccompState{{2, 1}, {2, 1}}, 0, true, 1, false},
		{[]seccompState{{2, 4095}}, 0, true, 4095, false},
		{[]seccompState{{2, 4096}}, 0, true, 0, true},
		{[]seccompState{{0, 0}, {2, 1}}, 0, true, 0, true},
		{[]seccompState{{2, 1}, {2, 2}}, 0, true, 0, true},
		{[]seccompState{{2, 2}, {2, 2}}, 2, true, 2, false},
		{[]seccompState{{2, 3}}, 2, true, 0, true},
		{[]seccompState{{2, 3}, {2, 4}}, 2, false, 3, false},
		{[]seccompState{{2, 1}}, 2, false, 0, true},
		{[]seccompState{{0, 0}}, 2, false, 0, true},
		{nil, 0, true, 0, true},
	} {
		c := seccompCounts{floor: tc.floor, exact: tc.exact}
		var err error
		for _, s := range tc.states {
			if err = c.add(s); err != nil {
				break
			}
		}
		var got uint32
		if err == nil {
			got, err = c.result()
		}
		if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
			t.Errorf("case %+v got %d %v", tc, got, err)
		}
	}
}

const monitorMountFixture = "31 22 0:28 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n"

func TestMonitorMounts(t *testing.T) {
	var manyLines strings.Builder
	for id := 1; id <= 4096; id++ {
		fmt.Fprintf(&manyLines, "%d 99999 0:20 / / rw - overlay overlay rw\n", id)
	}
	manyLines.WriteString("4097 99999 0:28 / /sys/fs/cgroup ro - cgroup2 cgroup rw\n")
	for _, text := range []string{monitorMountFixture, strings.Replace(monitorMountFixture, "cgroup2", "cgroup", 1), "22 1 0:20 / / rw,relatime shared:1 - overlay overlay rw\n" + monitorMountFixture} {
		if err := validateCgroupMounts([]byte(text)); err != nil {
			t.Fatalf("valid %q: %v", text, err)
		}
	}
	for name, text := range map[string]string{
		"empty": "", "missing": "22 1 0:20 / / rw - overlay overlay rw\n",
		"writable":          strings.Replace(monitorMountFixture, " ro,", " rw,", 1),
		"both":              strings.Replace(monitorMountFixture, " ro,", " ro,rw,", 1),
		"alternate":         monitorMountFixture + "32 22 0:28 / /alternate rw - cgroup2 cgroup rw\n",
		"duplicate":         monitorMountFixture + monitorMountFixture,
		"missing separator": strings.Replace(monitorMountFixture, " - ", " ", 1),
		"extra separator":   strings.Replace(monitorMountFixture, " - ", " - - ", 1),
		"bad ID":            strings.Replace(monitorMountFixture, "31 ", "x ", 1),
		"bad device":        strings.Replace(monitorMountFixture, "0:28", "0:x", 1),
		"bad root":          strings.Replace(monitorMountFixture, " / /", " relative /", 1),
		"empty option":      strings.Replace(monitorMountFixture, "ro,nosuid", "ro,,nosuid", 1),
		"duplicate option":  strings.Replace(monitorMountFixture, "ro,nosuid", "ro,ro,nosuid", 1),
		"fake ro":           strings.Replace(monitorMountFixture, "ro,nosuid", "xro,nosuid", 1),
		"truncated":         strings.TrimSuffix(monitorMountFixture, " rw\n") + "\n",
		"long line":         strings.Repeat("x", (16<<10)+1) + "\n",
		"too many lines":    manyLines.String(),
		"too many bytes":    strings.Repeat("x", (1<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCgroupMounts([]byte(text)); !errors.Is(err, ErrUnsafeKernel) {
				t.Fatalf("accepted/incorrect error: %v", err)
			}
		})
	}
}

func TestMonitorCgroup(t *testing.T) {
	for _, text := range []string{"0::/\n", "1:cpu,cpuacct:/slice\n2:memory:/other\n", "3:name=systemd:/a b:c\n"} {
		if err := validateCgroupMembership([]byte(text)); err != nil {
			t.Errorf("valid %q: %v", text, err)
		}
	}
	for _, text := range []string{"", "\n", "0::/", "0::/\n\n", "0::/\n0::/x\n", "1:cpu:/\n1:memory:/\n", "1:cpu,cpu:/\n", "1:cpu:/\n2:cpu:/\n", "0:cpu:/\n", "1::/\n", "-1:cpu:/\n", "01:cpu:/\n", "1:cpu:relative\n", "0::/\x00\n", "0::/\r\n", strings.Repeat("x", 4097)} {
		if err := validateCgroupMembership([]byte(text)); !errors.Is(err, ErrUnsafeKernel) {
			t.Errorf("accepted/incorrect error %q: %v", text[:min(len(text), 100)], err)
		}
	}
}

// Execute the generated cBPF instructions, not a second policy predicate.
func evaluateMonitorBPF(t *testing.T, p []unix.SockFilter, arch, nr uint32, flags uint64) uint32 {
	t.Helper()
	var data [64]byte
	binary.LittleEndian.PutUint32(data[0:], nr)
	binary.LittleEndian.PutUint32(data[4:], arch)
	binary.LittleEndian.PutUint64(data[16:], flags)
	var a uint32
	for pc := 0; pc < len(p); pc++ {
		ins := p[pc]
		switch ins.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			if ins.K > 60 {
				t.Fatal("invalid BPF offset")
			}
			a = binary.LittleEndian.Uint32(data[ins.K:])
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if a == ins.K {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			if a >= ins.K {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if a&ins.K != 0 {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return ins.K
		default:
			t.Fatalf("unknown BPF instruction %#x", ins.Code)
		}
	}
	t.Fatal("BPF ran off end")
	return 0
}

func TestMonitorPolicy(t *testing.T) {
	for _, tc := range []struct {
		arch    string
		audit   uint32
		denied  []uint32
		clone   uint32
		allowed []uint32
	}{
		{"amd64", 0xc000003e, []uint32{272, 308, 101, 310, 311}, 56, []uint32{0, 1, 39, 57, 58, 59, 60, 157, 202, 511, 548}},
		{"arm64", 0xc00000b7, []uint32{97, 268, 117, 270, 271}, 220, []uint32{63, 64, 93, 98, 167, 172, 221, 511, 548}},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			p, err := monitorPolicy(tc.arch)
			if err != nil {
				t.Fatal(err)
			}
			check := func(audit, nr uint32, flags uint64, want uint32) {
				t.Helper()
				if got := evaluateMonitorBPF(t, p, audit, nr, flags); got != want {
					t.Errorf("arch=%x nr=%d flags=%x got=%x want=%x", audit, nr, flags, got, want)
				}
			}
			for _, nr := range tc.denied {
				check(tc.audit, nr, 0, unix.SECCOMP_RET_ERRNO|uint32(unix.EACCES))
			}
			for _, flag := range []uint64{0x20000, 0x2000000, 0x4000000, 0x8000000, 0x10000000, 0x20000000, 0x40000000, 0x80} {
				check(tc.audit, tc.clone, flag|17, unix.SECCOMP_RET_ERRNO|uint32(unix.EACCES))
			}
			for _, flags := range []uint64{0, 17, 0x10f00, 0x4111, 0x80000000, 1 << 32} {
				check(tc.audit, tc.clone, flags, unix.SECCOMP_RET_ALLOW)
			}
			for _, nr := range tc.allowed {
				check(tc.audit, nr, ^uint64(0), unix.SECCOMP_RET_ALLOW)
			}
			check(tc.audit, 435, 0, unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS))
			for _, nr := range []uint32{0, tc.clone, 435, 512, 0x40000000} {
				check(0x40000003, nr, 0, unix.SECCOMP_RET_KILL_PROCESS)
			}
			if tc.arch == "amd64" {
				for _, nr := range []uint32{0, 56, 59, 435, 548} {
					check(tc.audit, nr|0x40000000, 0, unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS))
				}
				for nr := uint32(512); nr <= 547; nr++ {
					check(tc.audit, nr, 0, unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS))
				}
			}
		})
	}
	if _, err := monitorPolicy("386"); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestMonitorMountStrictFields(t *testing.T) {
	for name, data := range map[string]string{
		"raw NUL":                       strings.Replace(monitorMountFixture, "/sys/fs/cgroup", "/sys/fs/cg\x00roup", 1),
		"bad escape":                    strings.Replace(monitorMountFixture, "/sys/fs/cgroup", "/sys/fs/cg\\777roup", 1),
		"missing record newline":        strings.TrimSuffix(monitorMountFixture, "\n"),
		"missing mount access mode":     "22 1 0:20 / / relatime - overlay overlay rw\n" + monitorMountFixture,
		"conflicting mount access mode": "22 1 0:20 / / ro,rw - overlay overlay rw\n" + monitorMountFixture,
		"trailing space":                strings.TrimSuffix(monitorMountFixture, "\n") + " \n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCgroupMounts([]byte(data)); !errors.Is(err, ErrUnsafeKernel) {
				t.Fatalf("malformed mountinfo accepted: %v", err)
			}
		})
	}
	if err := validateCgroupMounts([]byte(strings.Replace(monitorMountFixture, "/sys/fs/cgroup", "/escaped\\040space\\011tab\\012newline\\134slash", 1))); err != nil {
		t.Fatalf("valid proc escapes: %v", err)
	}
}

// ScanLines drops a raw CR before LF. Strict proc parsing must reject that byte
// before token normalization, including the extra byte beyond the line bound.
func TestMonitorMountCRLF(t *testing.T) {
	line := strings.TrimSuffix(monitorMountFixture, "\n")
	const mountpoint = "/sys/fs/cgroup"
	boundary := strings.Replace(line, mountpoint, "/"+strings.Repeat("x", maxMountLineBytes-len(line)+len(mountpoint)-1), 1)
	for _, valid := range []string{line + "\n", boundary + "\n"} {
		if err := validateCgroupMounts([]byte(valid)); err != nil {
			t.Fatalf("valid LF mountinfo rejected: %v", err)
		}
	}
	for name, data := range map[string]string{
		"cgroup CRLF":               line + "\r\n",
		"other record CRLF":         "22 1 0:20 / / rw - overlay overlay rw\r\n" + monitorMountFixture,
		"CR exceeds raw line bound": boundary + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCgroupMounts([]byte(data)); !errors.Is(err, ErrUnsafeKernel) {
				t.Fatalf("raw CR accepted after line normalization: %v", err)
			}
		})
	}
}

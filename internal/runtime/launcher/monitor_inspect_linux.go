//go:build linux && (amd64 || arm64)

package launcher

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxCgroupBytes    = 4096
	maxMountBytes     = 1 << 20
	maxMountLines     = 4096
	maxMountLineBytes = 16 << 10
)

type seccompState struct{ mode, filters uint32 }

func parseSeccompStatus(data []byte) (seccompState, error) {
	var s seccompState
	if len(data) > maxStatusBytes {
		return s, fmt.Errorf("%w: status too large", ErrUnsafeKernel)
	}
	seen := uint8(0)
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), maxStatusBytes+1)
	for scan.Scan() {
		name, value, ok := strings.Cut(scan.Text(), ":")
		if !ok {
			continue
		}
		var bit uint8
		switch name {
		case "Seccomp":
			bit = 1
		case "Seccomp_filters":
			bit = 2
		default:
			continue
		}
		if seen&bit != 0 {
			return s, fmt.Errorf("%w: duplicate %s", ErrUnsafeKernel, name)
		}
		seen |= bit
		fields := strings.Fields(value)
		if len(fields) != 1 {
			return s, fmt.Errorf("%w: invalid %s", ErrUnsafeKernel, name)
		}
		n, err := parseUnsigned(fields[0], 10, 32)
		if err != nil {
			return s, fmt.Errorf("%w: %s: %w", ErrUnsafeKernel, name, err)
		}
		if bit == 1 {
			s.mode = uint32(n)
		} else {
			s.filters = uint32(n)
		}
	}
	if err := scan.Err(); err != nil {
		return s, fmt.Errorf("%w: status: %w", ErrUnsafeKernel, err)
	}
	if seen != 3 {
		return s, fmt.Errorf("%w: missing Seccomp/Seccomp_filters", ErrUnsupported)
	}
	if (s.mode != 0 && s.mode != 2) || (s.mode == 0 && s.filters != 0) || (s.mode == 2 && s.filters == 0) {
		return s, fmt.Errorf("%w: incompatible seccomp mode=%d filters=%d", ErrUnsafeKernel, s.mode, s.filters)
	}
	return s, nil
}

// floor=0 is the uniform pre-install baseline. exact post-install observations
// require floor; later validation allows any stronger stacked filter count.
type seccompCounts struct {
	floor uint32
	exact bool
	seen  bool
	first seccompState
}

func (c *seccompCounts) add(s seccompState) error {
	bad := false
	if c.floor == 0 {
		bad = s.filters >= 4096 || (c.seen && s != c.first)
	} else {
		bad = s.mode != 2 || s.filters < c.floor || (c.exact && s.filters != c.floor)
	}
	if bad {
		return fmt.Errorf("%w: nonuniform or incompatible seccomp count mode=%d filters=%d required=%d exact=%t", ErrUnsafeKernel, s.mode, s.filters, c.floor, c.exact)
	}
	if !c.seen {
		c.first = s
		c.seen = true
	}
	return nil
}
func (c *seccompCounts) result() (uint32, error) {
	if !c.seen {
		return 0, fmt.Errorf("%w: no observed seccomp threads", ErrKernelUnavailable)
	}
	return c.first.filters, nil
}

func inspectMonitorSeccomp(pid int, floor uint32, exact bool) (uint32, error) {
	for attempt := 0; attempt < 3; attempt++ {
		c := seccompCounts{floor: floor, exact: exact}
		_, err := walkKernelThreads(pid, func(tid int, _ KernelSnapshot) error {
			b, err := readBounded(fmt.Sprintf("/proc/%d/task/%d/status", pid, tid), maxStatusBytes)
			if err != nil {
				return err
			}
			s, err := parseSeccompStatus(b)
			if err != nil {
				return err
			}
			return c.add(s)
		})
		if err == nil {
			return c.result()
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			if attempt < 2 {
				continue
			}
		}
		if errors.Is(err, ErrUnsafeKernel) || errors.Is(err, ErrUnsupported) {
			return 0, err
		}
		return 0, unavailable("inspect monitor seccomp threads", err)
	}
	panic("unreachable")
}

func unsafeMount(reason string) error { return fmt.Errorf("%w: mountinfo %s", ErrUnsafeKernel, reason) }
func mountOptions(value string) (map[string]bool, error) {
	options := map[string]bool{}
	for _, v := range strings.Split(value, ",") {
		if v == "" || options[v] {
			return nil, unsafeMount("empty/duplicate option")
		}
		options[v] = true
	}
	return options, nil
}
func validateCgroupMounts(data []byte) error {
	if len(data) == 0 || len(data) > maxMountBytes {
		return unsafeMount("byte bound")
	}
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), maxMountLineBytes+2)
	ids := map[uint64]bool{}
	count, cgroups := 0, 0
	for scan.Scan() {
		count++
		line := scan.Text()
		if count > maxMountLines || len(line) > maxMountLineBytes {
			return unsafeMount("line bound")
		}
		f := strings.Fields(line)
		if len(f) < 10 {
			return unsafeMount("missing mandatory fields")
		}
		sep := -1
		for i, v := range f {
			if v == "-" {
				if sep != -1 {
					return unsafeMount("duplicate separator")
				}
				sep = i
			}
		}
		if sep < 6 || len(f) != sep+4 {
			return unsafeMount("invalid separator/tail")
		}
		id, err := parseUnsigned(f[0], 10, 32)
		if err != nil || id == 0 || ids[id] {
			return unsafeMount("invalid/duplicate mount ID")
		}
		ids[id] = true
		if _, err := parseUnsigned(f[1], 10, 32); err != nil {
			return unsafeMount("invalid parent ID")
		}
		major, minor, ok := strings.Cut(f[2], ":")
		if !ok {
			return unsafeMount("invalid device")
		}
		if _, err := parseUnsigned(major, 10, 32); err != nil {
			return unsafeMount("invalid major")
		}
		if _, err := parseUnsigned(minor, 10, 32); err != nil {
			return unsafeMount("invalid minor")
		}
		if !strings.HasPrefix(f[3], "/") || !strings.HasPrefix(f[4], "/") {
			return unsafeMount("nonabsolute root/mountpoint")
		}
		opts, err := mountOptions(f[5])
		if err != nil {
			return err
		}
		if _, err := mountOptions(f[sep+3]); err != nil {
			return err
		}
		if f[sep+1] == "cgroup" || f[sep+1] == "cgroup2" {
			cgroups++
			if !opts["ro"] || opts["rw"] {
				return unsafeMount("writable/ambiguous cgroup view")
			}
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("%w: mountinfo: %w", ErrUnsafeKernel, err)
	}
	if cgroups == 0 {
		return unsafeMount("missing cgroup mount")
	}
	return nil
}

func validateCgroupMembership(data []byte) error {
	bad := func() error { return fmt.Errorf("%w: malformed/ambiguous cgroup membership", ErrUnsafeKernel) }
	if len(data) == 0 || len(data) > maxCgroupBytes || data[len(data)-1] != '\n' {
		return bad()
	}
	ids := map[string]bool{}
	controllers := map[string]bool{}
	for _, line := range strings.Split(string(data[:len(data)-1]), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 {
			return bad()
		}
		id, err := parseUnsigned(f[0], 10, 31)
		if err != nil || strconv.FormatUint(id, 10) != f[0] || ids[f[0]] {
			return bad()
		}
		ids[f[0]] = true
		if (id == 0) != (f[1] == "") {
			return bad()
		}
		if f[1] != "" {
			for _, c := range strings.Split(f[1], ",") {
				if c == "" || controllers[c] || strings.ContainsAny(c, " \t\r\x00") {
					return bad()
				}
				controllers[c] = true
			}
		}
		if !strings.HasPrefix(f[2], "/") || strings.ContainsAny(f[2], "\x00\r") {
			return bad()
		}
	}
	return nil
}

func inspectMonitorCgroup() ([]byte, error) {
	mounts, err := readBounded("/proc/self/mountinfo", maxMountBytes)
	if err != nil {
		return nil, unavailable("monitor mountinfo", err)
	}
	if err = validateCgroupMounts(mounts); err != nil {
		return nil, err
	}
	membership, err := readBounded("/proc/self/cgroup", maxCgroupBytes)
	if err != nil {
		return nil, unavailable("monitor cgroup", err)
	}
	if err = validateCgroupMembership(membership); err != nil {
		return nil, err
	}
	return membership, nil
}

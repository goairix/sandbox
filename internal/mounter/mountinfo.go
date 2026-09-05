package mounter

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const maxMountInfoBytes = 4 << 20

type Mount struct {
	ID             uint64
	ParentID       uint64
	Root           string
	MountPoint     string
	FilesystemType string
	Source         string
}

type MountIDResolver func(path string) (uint64, error)

func ParseMountInfo(reader io.Reader) ([]Mount, error) {
	limited := io.LimitReader(reader, maxMountInfoBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	if len(raw) > maxMountInfoBytes {
		return nil, fmt.Errorf("mountinfo exceeds size limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("mountinfo is empty")
	}
	var result []Mount
	ids := make(map[uint64]Mount)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if len(fields) < 10 || separator < 6 || separator+3 >= len(fields) {
			return nil, fmt.Errorf("mountinfo contains malformed record")
		}
		id, idErr := strconv.ParseUint(fields[0], 10, 64)
		parent, parentErr := strconv.ParseUint(fields[1], 10, 64)
		root, rootErr := unescapeMountInfo(fields[3])
		mountPoint, pointErr := unescapeMountInfo(fields[4])
		source, sourceErr := unescapeMountInfo(fields[separator+2])
		if idErr != nil || parentErr != nil || rootErr != nil || pointErr != nil || sourceErr != nil || fields[separator+1] == "" {
			return nil, fmt.Errorf("mountinfo contains malformed record")
		}
		mount := Mount{ID: id, ParentID: parent, Root: root, MountPoint: mountPoint, FilesystemType: fields[separator+1], Source: source}
		if previous, duplicate := ids[id]; duplicate && previous != mount {
			return nil, fmt.Errorf("mountinfo contains conflicting mount IDs")
		}
		if _, duplicate := ids[id]; !duplicate {
			ids[id] = mount
			result = append(result, mount)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mountinfo: %w", err)
	}
	return result, nil
}

func EffectiveMount(reader io.Reader, path string, resolve MountIDResolver) (Mount, error) {
	if resolve == nil {
		return Mount{}, fmt.Errorf("mount ID resolver is unavailable")
	}
	id, err := resolve(path)
	if err != nil {
		return Mount{}, fmt.Errorf("resolve effective mount ID: %w", err)
	}
	mounts, err := ParseMountInfo(reader)
	if err != nil {
		return Mount{}, err
	}
	for _, mount := range mounts {
		if mount.ID == id && mount.MountPoint == path {
			return mount, nil
		}
	}
	return Mount{}, fmt.Errorf("effective mount is absent from mountinfo")
}

func ReadEffectiveMount(path string) (Mount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return Mount{}, fmt.Errorf("open mountinfo: %w", err)
	}
	defer file.Close()
	return EffectiveMount(file, path, kernelMountID)
}

func unescapeMountInfo(value string) (string, error) {
	var decoded strings.Builder
	decoded.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			decoded.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", fmt.Errorf("truncated mountinfo escape")
		}
		escape := value[i+1 : i+4]
		switch escape {
		case "040":
			decoded.WriteByte(' ')
		case "011":
			decoded.WriteByte('\t')
		case "012":
			decoded.WriteByte('\n')
		case "134":
			decoded.WriteByte('\\')
		default:
			return "", fmt.Errorf("unsupported mountinfo escape")
		}
		i += 3
	}
	return decoded.String(), nil
}

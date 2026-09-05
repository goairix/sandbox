package workspaceprobe

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type procMountInfo struct{ path string }

func (p procMountInfo) requireS3FSMount(path string) error {
	mountID, err := effectiveMountID(path)
	if err != nil {
		return err
	}
	file, err := os.Open(p.path)
	if err != nil {
		return err
	}
	defer file.Close()
	return requireEffectiveS3FS(file, path, mountID)
}

func requireEffectiveS3FS(reader io.Reader, path string, effectiveMountID uint64) error {
	path = filepath.Clean(path)
	found := false
	limited := &io.LimitedReader{R: reader, N: fuseprotocolMountInfoLimit + 1}
	scanner := bufio.NewScanner(limited)
	buffer := make([]byte, 4096)
	scanner.Buffer(buffer, fuseprotocolMountInfoLineLimit)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if len(fields) < 7 || separator < 0 || separator+2 >= len(fields) {
			return fmt.Errorf("malformed mountinfo entry")
		}
		mountID, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || mountID == 0 {
			return fmt.Errorf("malformed mountinfo id")
		}
		if mountID != effectiveMountID {
			continue
		}
		mountpoint, err := decodeMountInfoPath(fields[4])
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("duplicate effective mount id")
		}
		found = true
		if filepath.Clean(mountpoint) != path || fields[separator+1] != "fuse.s3fs" {
			return fmt.Errorf("effective workspace mount has wrong path or type")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if limited.N == 0 {
		return fmt.Errorf("mountinfo exceeds size limit")
	}
	if !found {
		return fmt.Errorf("effective workspace mount id is missing")
	}
	return nil
}

const (
	fuseprotocolMountInfoLimit     = 4 << 20
	fuseprotocolMountInfoLineLimit = 1 << 20
)

func decodeMountInfoPath(value string) (string, error) {
	var result strings.Builder
	result.Grow(len(value))
	for i := 0; i < len(value); {
		if value[i] != '\\' {
			result.WriteByte(value[i])
			i++
			continue
		}
		if i+3 >= len(value) {
			return "", fmt.Errorf("malformed mountinfo escape")
		}
		octal := value[i+1 : i+4]
		decoded, err := strconv.ParseUint(octal, 8, 8)
		if err != nil || (octal != "040" && octal != "011" && octal != "012" && octal != "134") {
			return "", fmt.Errorf("unsupported mountinfo escape")
		}
		result.WriteByte(byte(decoded))
		i += 4
	}
	return result.String(), nil
}

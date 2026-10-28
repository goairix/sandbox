//go:build !linux

package filev2

import "errors"

func openBeneath(_ int, _ string, _ int, _ uint32) (int, error) {
	return -1, errors.New("sandbox file operations require Linux openat2")
}

func openWriteWithParents(_ int, _ string) (int, error) {
	return -1, errors.New("sandbox file operations require Linux openat2")
}

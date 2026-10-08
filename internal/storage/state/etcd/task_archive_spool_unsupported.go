//go:build !linux && !darwin

package etcd

import "os"

type taskArchiveIdentity struct{ dev, ino uint64 }

func taskArchiveOpenRoot(string) (*os.File, error) { return nil, errTaskArchive }
func taskArchiveCreate(*os.File, string) (*os.File, taskArchiveIdentity, error) {
	return nil, taskArchiveIdentity{}, errTaskArchive
}
func taskArchiveCheck(*os.File, string, *os.File, taskArchiveIdentity) error  { return errTaskArchive }
func taskArchiveRemove(*os.File, string, *os.File, taskArchiveIdentity) error { return errTaskArchive }

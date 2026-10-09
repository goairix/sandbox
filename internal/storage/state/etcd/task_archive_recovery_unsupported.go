//go:build !linux

package etcd

import "os"

func taskArchiveRecoverySupported() error { return errTaskArchive }
func taskArchiveRecoveryOpenRoot(string) (*os.File, taskArchiveRecoveryRoot, error) {
	return nil, taskArchiveRecoveryRoot{}, errTaskArchive
}
func taskArchiveRecoveryCheckRoot(*os.File, string, taskArchiveRecoveryRoot) error {
	return errTaskArchive
}
func taskArchiveRecoveryOpen(*os.File, string) (*os.File, taskArchiveIdentity, error) {
	return nil, taskArchiveIdentity{}, errTaskArchive
}
func taskArchiveRecoveryCommit(*os.File, string, []byte) (taskArchiveIdentity, error) {
	return taskArchiveIdentity{}, errTaskArchive
}

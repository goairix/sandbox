//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package mounter

import "os"

func ownedByRoot(os.FileInfo) bool        { return false }
func ownedByCurrentUser(os.FileInfo) bool { return false }

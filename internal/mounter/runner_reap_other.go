//go:build !linux

package mounter

func reapOrphanedProcessGroup(int) {}

//go:build !linux

package main

import "github.com/goairix/sandbox/internal/runtime/controlrunner"

func provisionProtectedConfig([]byte) error { return controlrunner.ErrUnavailable }

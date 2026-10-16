package main

import (
	"context"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
)

func runEtcdStartupCheck(ctx context.Context, supplied map[string]bool, args []string, path string, check func(context.Context, string) error) (bool, error) {
	if !supplied["check-etcd-startup"] {
		return false, nil
	}
	if path == "" || len(args) != 0 {
		return true, errors.New("etcd startup check requires one manifest path and no positional arguments")
	}
	for name := range supplied {
		if name != "check-etcd-startup" {
			return true, errors.New("etcd startup check cannot be combined with other startup flags")
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return true, check(bounded, path)
}

func checkEtcdStartup(ctx context.Context, path string) error {
	d, err := deployment.Open(ctx, path)
	if err != nil {
		return err
	}
	return d.Close()
}

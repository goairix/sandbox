package main

import (
	"context"
	"errors"
	"net"
	"strconv"

	"github.com/goairix/sandbox/internal/api/etcdgateway"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
)

func runEtcdServe(ctx context.Context, supplied map[string]bool, args []string, path string, serve func(context.Context, string) error) (bool, error) {
	if !supplied["serve-etcd"] {
		return false, nil
	}
	if path == "" || len(args) != 0 {
		return true, errors.New("etcd HTTP serve requires one manifest path and no positional arguments")
	}
	for name := range supplied {
		if name != "serve-etcd" {
			return true, errors.New("etcd HTTP serve cannot be combined with other startup flags")
		}
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return true, serve(ctx, path)
}

func serveEtcd(ctx context.Context, path string) (err error) {
	owner, err := deployment.OpenHTTP(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, owner.Close()) }()
	settings := owner.HTTPSettings()
	if settings == nil {
		return errors.New("etcd HTTP settings required")
	}
	defer settings.Zero()
	gateway, err := etcdgateway.New(owner.Service(), settings)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := gateway.Close()
		if closeErr != nil && !errors.Is(err, closeErr) {
			err = errors.Join(err, closeErr)
		}
	}()
	listener, err := net.Listen("tcp", net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)))
	if err != nil {
		return errors.New("etcd HTTP listener unavailable")
	}
	return etcdgateway.Serve(ctx, listener, gateway)
}

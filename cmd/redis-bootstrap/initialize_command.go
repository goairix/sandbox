package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/goairix/sandbox/internal/redisbootstrap"
	"k8s.io/apimachinery/pkg/util/validation"

	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type initializeClientFactory func(context.Context, string) (corev1.ConfigMapInterface, error)

const initializeUsage = "usage: redis-bootstrap initialize -state-configmap NAME [-namespace NAME] [public file options]\npasswords: REDIS_PASSWORD and REDIS_SENTINEL_PASSWORD environment only; namespace fallback: POD_NAMESPACE\n"

func runInitialize(ctx context.Context, args []string, out io.Writer) error {
	return runInitializeWithClient(ctx, args, out, func(ctx context.Context, namespace string) (corev1.ConfigMapInterface, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, errControlFiles
		}
		config.Timeout = 5 * time.Second
		config.QPS = 2
		config.Burst = 3
		client, err := corev1.NewForConfig(config)
		if err != nil {
			return nil, errControlFiles
		}
		return client.ConfigMaps(namespace), nil
	})
}

func runInitializeWithClient(ctx context.Context, args []string, out io.Writer, factory initializeClientFactory) error {
	if ctx == nil || out == nil || factory == nil {
		return errArguments
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	flags := flag.NewFlagSet("initialize", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	namespace := flags.String("namespace", os.Getenv("POD_NAMESPACE"), "release namespace")
	name := flags.String("state-configmap", "", "existing release state ConfigMap")
	public := flags.String("public-keys-file", "/identity-public/public-keys.json", "three public keys only")
	master := flags.String("master-name", "sandbox", "fixed Sentinel master name")
	timeout := flags.Duration("timeout", 12*time.Minute, "bounded initialize wait")
	ack := flags.Duration("ack-timeout", time.Second, "new-write replication wait")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) && len(args) == 1 {
			if _, err := io.WriteString(out, initializeUsage); err != nil {
				return errArguments
			}
			return nil
		}
		return errArguments
	}
	dataPassword, sentinelPassword := os.Getenv("REDIS_PASSWORD"), os.Getenv("REDIS_SENTINEL_PASSWORD")
	if dataPassword == sentinelPassword {
		return errArguments
	}
	if flags.NArg() != 0 || len(validation.IsDNS1123Label(*namespace)) != 0 || len(validation.IsDNS1123Subdomain(*name)) != 0 || net.ParseIP(*name) != nil || !filepath.IsAbs(*public) || filepath.Clean(*public) == "/" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(*master) || *timeout <= 0 || *timeout > 12*time.Minute || *ack < time.Second || *ack > 10*time.Second || redisbootstrap.ValidateBuiltinSentinelPassword(dataPassword) != nil || redisbootstrap.ValidateBuiltinSentinelPassword(sentinelPassword) != nil {
		return errArguments
	}
	data, err := readControlFile(ctx, *public, 1024, false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errControlFiles
	}
	keys, err := redisbootstrap.ParseMemberPublicKeys(data)
	if err != nil {
		return errControlFiles
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client, err := factory(ctx, *namespace)
	if err != nil || client == nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errControlFiles
	}
	return redisbootstrap.InitializeNamespaceBootstrap(ctx, client, redisbootstrap.InitializeOptions{Namespace: *namespace, Name: *name, PublicKeys: keys, MasterName: *master, DataPassword: dataPassword, SentinelPassword: sentinelPassword, AckTimeout: *ack, Timeout: *timeout})
}

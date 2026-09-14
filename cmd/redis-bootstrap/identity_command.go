package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/goairix/sandbox/internal/redisbootstrap"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type identityClientFactory func(context.Context) (corev1.CoreV1Interface, error)

const ensureIdentityUsage = "usage: redis-bootstrap ensure-identity -statefulset NAME -secret NAME -state-configmap NAME -members-json JSON [-namespace NAME] [-fresh-cluster-id ID] [-timeout 2m]\nnamespace fallback: POD_NAMESPACE; empty fresh-cluster-id verifies existing identity only; success is silent\n"

func runEnsureIdentity(ctx context.Context, args []string, out io.Writer) error {
	return runEnsureIdentityWithClient(ctx, args, out, func(ctx context.Context) (corev1.CoreV1Interface, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, redisbootstrap.ErrIdentityAPI
		}
		config.Timeout = 5 * time.Second
		config.QPS = 2
		config.Burst = 3
		config.WarningHandlerWithContext = rest.NoWarnings{}
		client, err := corev1.NewForConfig(config)
		if err != nil {
			return nil, redisbootstrap.ErrIdentityAPI
		}
		return client, nil
	})
}

func runEnsureIdentityWithClient(ctx context.Context, args []string, out io.Writer, factory identityClientFactory) error {
	if ctx == nil || out == nil || factory == nil {
		return errArguments
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	flags := flag.NewFlagSet("ensure-identity", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	namespace := flags.String("namespace", "", "release namespace")
	sts := flags.String("statefulset", "", "fixed StatefulSet name")
	secret := flags.String("secret", "", "fixed identity Secret name")
	state := flags.String("state-configmap", "", "fixed bootstrap state ConfigMap")
	members := flags.String("members-json", "", "fixed three member DNS array")
	fresh := flags.String("fresh-cluster-id", "", "new install creation permit only")
	timeout := flags.Duration("timeout", 2*time.Minute, "bounded identity wait")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) && len(args) == 1 {
			if _, err := io.WriteString(out, ensureIdentityUsage); err != nil {
				return errArguments
			}
			return nil
		}
		return errArguments
	}
	if flags.NArg() != 0 || *timeout <= 0 || len(*members) > 1024 {
		return errArguments
	}
	if *namespace == "" {
		*namespace = os.Getenv("POD_NAMESPACE")
	}
	var dns []string
	if json.Unmarshal([]byte(*members), &dns) != nil || len(dns) != 3 {
		return errArguments
	}
	o := redisbootstrap.IdentitySecretOptions{Namespace: *namespace, StatefulSetName: *sts, SecretName: *secret, StateConfigMap: *state, Members: [3]string{dns[0], dns[1], dns[2]}, FreshClusterID: *fresh, Timeout: *timeout}
	if o.Validate() != nil {
		return errArguments
	}
	client, err := factory(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || client == nil {
		return redisbootstrap.ErrIdentityAPI
	}
	return redisbootstrap.EnsureIdentitySecret(ctx, client, o)
}

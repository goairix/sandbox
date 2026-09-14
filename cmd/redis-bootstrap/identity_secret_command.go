package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/goairix/sandbox/internal/redisbootstrap"
	"io"
)

var errIdentitySecretOutput = errors.New("identity secret output failed")

// This manual command prints a new immutable Secret only. It does not contact
// Kubernetes and must never be automatically replayed for an existing volume group.
func runIdentitySecret(ctx context.Context, args []string, output io.Writer) error {
	if ctx == nil || output == nil {
		return errArguments
	}
	flags := flag.NewFlagSet("identity-secret", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	namespace := flags.String("namespace", "", "target namespace")
	statefulset := flags.String("statefulset", "", "fixed StatefulSet name")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag.Parse stops at help; do not let it hide unknown options or
			// misplaced credentials later in this invocation. Help is standalone.
			if len(args) != 1 {
				return errArguments
			}
			if _, err := io.WriteString(output, "usage: redis-bootstrap identity-secret -namespace NAME -statefulset NAME\nPrint a NEW immutable identity Secret as JSON; preserve existing Secret and PVC identities.\n"); err != nil {
				return errIdentitySecretOutput
			}
			return nil
		}
		return errArguments
	}
	if flags.NArg() != 0 || *namespace == "" || *statefulset == "" {
		return errArguments
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	secret, err := redisbootstrap.GenerateIdentitySecret(*namespace, *statefulset)
	if err != nil {
		return errArguments
	}
	secret.APIVersion = "v1"
	secret.Kind = "Secret"
	defer func() {
		for name, data := range secret.Data {
			if name != "public-keys.json" {
				for i := range data {
					data[i] = 0
				}
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(secret); err != nil {
		return errIdentitySecretOutput
	}
	return ctx.Err()
}

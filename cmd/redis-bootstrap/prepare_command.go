package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/goairix/sandbox/internal/redisbootstrap"
)

func runPrepare(ctx context.Context, args []string, output io.Writer) error {
	if ctx == nil {
		return errArguments
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o, err := parsePrepareOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err := io.WriteString(output, "usage: redis-bootstrap prepare-pod [-ordinal 0|1|2] [-uid 999] [-gid 999]\nroot-only one-time preparation; fixed pod mounts and image tooling; ordinal fallback: POD_ORDINAL\n")
		return err
	}
	if err != nil {
		return errArguments
	}
	return redisbootstrap.PreparePod(ctx, o)
}

func parsePrepareOptions(args []string) (redisbootstrap.PreparePodOptions, error) {
	flags := flag.NewFlagSet("prepare-pod", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	ordinal := flags.Int("ordinal", -1, "fixed member ordinal")
	uid := flags.Int("uid", 999, "long-lived nonroot UID")
	gid := flags.Int("gid", 999, "long-lived nonroot GID")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return redisbootstrap.PreparePodOptions{}, flag.ErrHelp
		}
		return redisbootstrap.PreparePodOptions{}, errArguments
	}
	explicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "ordinal" {
			explicit = true
		}
	})
	if !explicit {
		var err error
		*ordinal, err = podOrdinal()
		if err != nil {
			return redisbootstrap.PreparePodOptions{}, errArguments
		}
	}
	if flags.NArg() != 0 || *ordinal < 0 || *ordinal > 2 || *uid < 1 || *uid > 2147483647 || *gid < 1 || *gid > 2147483647 {
		return redisbootstrap.PreparePodOptions{}, errArguments
	}
	return redisbootstrap.PreparePodOptions{Ordinal: *ordinal, UID: *uid, GID: *gid}, nil
}

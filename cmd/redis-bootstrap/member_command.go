package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/goairix/sandbox/internal/redisbootstrap"
)

type memberExec func(string, []string, []string) error

func runMember(ctx context.Context, args []string, output io.Writer) error {
	return runMemberWithExec(ctx, args, output, syscall.Exec)
}

func runMemberWithExec(ctx context.Context, args []string, output io.Writer, exec memberExec) error {
	if ctx == nil || len(args) == 0 || (args[0] != "redis" && args[0] != "sentinel") {
		return errArguments
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("data-dir", "/data", "trusted local PVC")
	cluster := flags.String("cluster-file", "/bootstrap/cluster.json", "fixed cluster state")
	registration := flags.String("registration-file", "/bootstrap/registration.json", "registered member markers")
	keys := flags.String("public-keys-file", "/identity-public/public-keys.json", "fixed public member keys")
	ordinal := flags.Int("ordinal", -1, "fixed member ordinal")
	master := flags.String("master-name", "sandbox", "fixed Sentinel master name")
	timeout := flags.Duration("timeout", 10*time.Minute, "bounded startup wait")
	down := flags.Int("down-after-milliseconds", 10000, "initial Sentinel failure threshold")
	failover := flags.Int("failover-timeout-milliseconds", 60000, "initial Sentinel failover timeout")
	parallel := flags.Int("parallel-syncs", 1, "initial Sentinel sync bound")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := io.WriteString(output, "usage: redis-bootstrap redis|sentinel [-ordinal 0|1|2] [local file options]\npasswords: REDIS_PASSWORD and REDIS_SENTINEL_PASSWORD environment only; ordinal fallback: POD_ORDINAL\n")
			return err
		}
		return errArguments
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
			return errArguments
		}
	}
	if flags.NArg() != 0 || *ordinal < 0 || *ordinal > 2 || exec == nil {
		return errArguments
	}
	o := redisbootstrap.MemberStartupOptions{Files: redisbootstrap.BootstrapFilePaths{Cluster: *cluster, Registration: *registration, PublicKeys: *keys}, Directory: *directory, Ordinal: *ordinal, Timeout: *timeout, Initial: redisbootstrap.InitialConfigOptions{MasterName: *master, DataPassword: os.Getenv("REDIS_PASSWORD"), SentinelPassword: os.Getenv("REDIS_SENTINEL_PASSWORD"), DownAfterMilliseconds: *down, FailoverTimeoutMilliseconds: *failover, ParallelSyncs: *parallel}}
	if redisbootstrap.ValidateBuiltinSentinelPassword(o.Initial.DataPassword) != nil || redisbootstrap.ValidateBuiltinSentinelPassword(o.Initial.SentinelPassword) != nil {
		return errArguments
	}
	path, err := redisbootstrap.PrepareMemberLaunch(ctx, o, args[0] == "sentinel")
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	argv := []string{"redis-server", path}
	if args[0] == "sentinel" {
		argv = append(argv, "--sentinel")
	}
	syscall.Umask(0077)
	if err := exec("/usr/local/bin/redis-server", argv, os.Environ()); err != nil {
		return errArguments
	}
	return nil
}

func podOrdinal() (int, error) {
	value := os.Getenv("POD_ORDINAL")
	if value != "0" && value != "1" && value != "2" {
		return 0, errArguments
	}
	ordinal, err := strconv.Atoi(value)
	if err != nil {
		return 0, errArguments
	}
	return ordinal, nil
}

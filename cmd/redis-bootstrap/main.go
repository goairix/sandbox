package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/goairix/sandbox/internal/redisbootstrap"
)

var errArguments = errors.New("invalid redis-bootstrap arguments")
var errControlFiles = errors.New("invalid Redis bootstrap identity files")
var errBootstrapRollback = errors.New("built-in Redis Sentinel rollback is disabled")

const usage = "usage: redis-bootstrap attestor [-ordinal 0|1|2] [local file options]\npassword: REDIS_PASSWORD environment only; identity service port: 18080\n"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var output io.Writer = os.Stderr
	if len(os.Args) > 1 && os.Args[1] == "identity-secret" {
		output = os.Stdout
	}
	if err := run(ctx, os.Args[1:], output); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "redis-bootstrap failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	// Helm rollback replays historical manifests without lookup and can erase
	// the one-time grant. Deny before credentials, files or API access.
	if len(args) > 0 && args[0] == "deny-rollback" {
		if ctx == nil || output == nil || len(args) != 1 {
			return errArguments
		}
		if _, err := io.WriteString(output, "built-in Redis Sentinel rollback is disabled; use helm upgrade to preserve initialized state\n"); err != nil {
			return errArguments
		}
		return errBootstrapRollback
	}
	if len(args) > 0 && args[0] == "initialize" {
		return runInitialize(ctx, args[1:], output)
	}
	if len(args) > 0 && args[0] == "prepare-pod" {
		return runPrepare(ctx, args[1:], output)
	}
	if len(args) > 0 && (args[0] == "redis" || args[0] == "sentinel") {
		return runMember(ctx, args, output)
	}
	if len(args) > 0 && args[0] == "identity-secret" {
		return runIdentitySecret(ctx, args[1:], output)
	}
	return runWithServer(ctx, args, output, redisbootstrap.RunIdentityServer)
}

func runWithServer(ctx context.Context, args []string, output io.Writer, serve func(context.Context, redisbootstrap.IdentityServerOptions) error) (resultErr error) {
	if ctx == nil {
		return errArguments
	}
	defer func() {
		if resultErr != nil && ctx.Err() != nil {
			resultErr = ctx.Err()
		}
	}()
	if len(args) == 0 || args[0] != "attestor" || serve == nil {
		return errArguments
	}
	flags := flag.NewFlagSet("attestor", flag.ContinueOnError)
	// Flag diagnostics can echo operator-supplied arguments; expose static help
	// and categories only, never raw input or a possible misplaced credential.
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", "/data", "trusted local PVC")
	clusterPath := flags.String("cluster-file", "/bootstrap/cluster.json", "fixed cluster state")
	registrationPath := flags.String("registration-file", "/bootstrap/registration.json", "retained member registration")
	publicPath := flags.String("public-keys-file", "/identity-public/public-keys.json", "three public keys")
	seedPath := flags.String("private-seed-file", "/identity-private/seed", "own member seed only")
	ordinal := flags.Int("ordinal", -1, "fixed member ordinal")
	masterName := flags.String("master-name", "sandbox", "fixed Sentinel master name")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := io.WriteString(output, usage)
			return err
		}
		return errArguments
	}
	explicitOrdinal := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "ordinal" {
			explicitOrdinal = true
		}
	})
	if !explicitOrdinal {
		value, err := podOrdinal()
		if err != nil {
			return errArguments
		}
		*ordinal = value
	}
	if flags.NArg() != 0 || *ordinal < 0 || *ordinal > 2 {
		return errArguments
	}
	for _, path := range []string{*dataDir, *clusterPath, *registrationPath, *publicPath, *seedPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return errArguments
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	clusterJSON, err := readControlFile(ctx, *clusterPath, 64<<10, false)
	if err != nil {
		return errControlFiles
	}
	cluster, err := redisbootstrap.ParseClusterState(clusterJSON)
	if err != nil {
		return errControlFiles
	}
	publicJSON, err := readControlFile(ctx, *publicPath, 1024, false)
	if err != nil {
		return errControlFiles
	}
	keys, err := redisbootstrap.ParseMemberPublicKeys(publicJSON)
	if err != nil {
		return errControlFiles
	}
	seed, err := readControlFile(ctx, *seedPath, 32, true)
	if err != nil {
		return errControlFiles
	}
	private, err := redisbootstrap.ParseMemberPrivateSeed(seed, keys, *ordinal)
	// This is best-effort removal of the explicit seed copy, not a guarantee of
	// erasing compiler/runtime copies. The handler intentionally retains its key.
	for i := range seed {
		seed[i] = 0
	}
	if err != nil {
		return errControlFiles
	}
	defer func() {
		for i := range private {
			private[i] = 0
		}
	}()
	registrationJSON, err := readControlFile(ctx, *registrationPath, 64<<10, false)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || cluster.Phase != redisbootstrap.Pending {
			return errControlFiles
		}
	} else {
		registration, err := redisbootstrap.ParseBootstrapRegistration(registrationJSON)
		if err != nil || registration.Cluster != cluster {
			return errControlFiles
		}
		digest, err := redisbootstrap.PublicKeySetDigest(keys)
		if err != nil || digest != registration.KeyDigest {
			return errControlFiles
		}
	}
	options := redisbootstrap.IdentityServerOptions{Observer: redisbootstrap.LocalObserverOptions{Directory: *dataDir, Cluster: cluster, Member: redisbootstrap.Member{DNS: cluster.Members[*ordinal], Ordinal: *ordinal}, MasterName: *masterName, Password: os.Getenv("REDIS_PASSWORD")}, PrivateKey: private}
	if _, err := redisbootstrap.NewLocalObserver(options.Observer); err != nil {
		return errArguments
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return serve(ctx, options)
}

// Control-plane projections are trusted Kubernetes mounts, not PVC state files.
// Their ..data symlinks are expected. O_NONBLOCK prevents a wrong FIFO mount from
// blocking before the regular-file check; reads have byte, not kernel I/O limits.
func readControlFile(ctx context.Context, path string, limit int64, private bool) (data []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, errControlFiles
	}
	defer func() {
		if err := file.Close(); err != nil {
			data = nil
			resultErr = errControlFiles
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errControlFiles
	}
	if private && (info.Mode() != info.Mode().Perm() || info.Mode().Perm()&0137 != 0 || info.Mode().Perm()&0440 == 0) {
		return nil, errControlFiles
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, errControlFiles
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

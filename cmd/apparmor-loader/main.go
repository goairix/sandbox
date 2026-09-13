package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goairix/sandbox/internal/apparmorloader"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("apparmor-loader: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) > 0 && args[0] == "readiness" {
		flags := flag.NewFlagSet("readiness", flag.ContinueOnError)
		flags.SetOutput(output)
		path := flags.String("readiness-file", "/run/apparmor-loader/ready", "recent successful verification marker")
		proc := flags.String("proc-root", "/proc", "container process filesystem")
		age := flags.Duration("max-age", 30*time.Second, "maximum successful-check age")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("readiness accepts no positional arguments")
		}
		return apparmorloader.Ready(*path, *proc, *age)
	}
	flags := flag.NewFlagSet("apparmor-loader", flag.ContinueOnError)
	flags.SetOutput(output)
	cfg := apparmorloader.Config{}
	flags.StringVar(&cfg.ProfilePath, "profile-path", "/etc/sandbox-apparmor/profile", "trusted rendered profile")
	flags.StringVar(&cfg.ProfileName, "profile-name", "", "exact sandbox-fuse content-addressed name")
	flags.StringVar(&cfg.ProfileDigest, "profile-digest", "", "canonical template SHA256")
	flags.StringVar(&cfg.ModuleEnabledPath, "module-enabled-path", "/sys/module/apparmor/parameters/enabled", "AppArmor module enable parameter")
	flags.StringVar(&cfg.ProfilesPath, "profiles-path", "/sys/kernel/security/apparmor/profiles", "securityfs loaded-profile list")
	flags.StringVar(&cfg.ReadinessFile, "readiness-file", "/run/apparmor-loader/ready", "success marker in private writable runtime directory")
	flags.StringVar(&cfg.ProcRoot, "proc-root", "/proc", "container process filesystem")
	flags.DurationVar(&cfg.CheckInterval, "check-interval", 10*time.Second, "profile recheck interval")
	flags.DurationVar(&cfg.ParserTimeout, "parser-timeout", 10*time.Second, "individual trusted parser timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("loader accepts no positional or tenant arguments")
	}
	loader, err := apparmorloader.New(cfg, apparmorloader.ExecParser)
	if err != nil {
		return fmt.Errorf("invalid loader configuration: %w", err)
	}
	return loader.Run(ctx)
}

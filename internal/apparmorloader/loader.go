package apparmorloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ProfilePath, ProfileName, ProfileDigest                  string
	ModuleEnabledPath, ProfilesPath, ReadinessFile, ProcRoot string
	CheckInterval, ParserTimeout                             time.Duration
}

// Parser accepts only trusted policy bytes and a fixed syntax/load operation.
type Parser func(context.Context, []byte, bool) error

type Loader struct {
	cfg           Config
	parser        Parser
	mu            sync.Mutex
	syntaxChecked bool
	policy        []byte
}

func New(cfg Config, parser Parser) (*Loader, error) {
	if parser == nil || cfg.CheckInterval <= 0 || cfg.ParserTimeout <= 0 {
		return nil, errors.New("parser and positive check interval/timeout required")
	}
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	for _, p := range []string{cfg.ProfilePath, cfg.ModuleEnabledPath, cfg.ProfilesPath, cfg.ReadinessFile, cfg.ProcRoot} {
		if !filepath.IsAbs(p) {
			return nil, errors.New("loader paths must be absolute")
		}
	}
	if cfg.ReadinessFile == cfg.ProfilePath || cfg.ReadinessFile == cfg.ProfilesPath || cfg.ReadinessFile == cfg.ModuleEnabledPath {
		return nil, errors.New("readiness file must be separate from policy and kernel paths")
	}
	policy, err := os.ReadFile(cfg.ProfilePath)
	if err != nil {
		return nil, errors.New("cannot read profile")
	}
	if err = ValidatePolicy(policy, cfg.ProfileName, cfg.ProfileDigest); err != nil {
		return nil, err
	}
	return &Loader{cfg: cfg, parser: parser, policy: CanonicalPolicy(policy)}, nil
}

// Check invalidates previous readiness before doing any potentially blocking work.
// Exact-name kernel entries attest mode, not contents: trusted node administrators
// must preserve content-addressed naming. Existing entries are NEVER replaced.
func (l *Loader) Check(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.clearReady(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	enabled, err := os.ReadFile(l.cfg.ModuleEnabledPath)
	if err != nil || strings.TrimSpace(string(enabled)) != "Y" {
		return errors.New("AppArmor module is unavailable or disabled")
	}
	policy, err := os.ReadFile(l.cfg.ProfilePath)
	if err != nil {
		return errors.New("cannot read mounted profile")
	}
	if err = ValidatePolicy(policy, l.cfg.ProfileName, l.cfg.ProfileDigest); err != nil {
		return err
	}
	if !bytes.Equal(CanonicalPolicy(policy), l.policy) {
		return errors.New("mounted profile changed")
	}
	if !l.syntaxChecked {
		if err = l.parse(ctx, false); err != nil {
			return fmt.Errorf("profile syntax check failed: %w", err)
		}
		l.syntaxChecked = true
	}
	found, err := l.enforced()
	if err != nil {
		return err
	}
	if !found {
		if err = l.parse(ctx, true); err != nil {
			return fmt.Errorf("profile load failed: %w", err)
		}
		found, err = l.enforced()
		if err != nil {
			return err
		}
		if !found {
			return errors.New("profile remains absent after parser load")
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return writeReadiness(l.cfg.ReadinessFile, l.cfg.ProcRoot, l.cfg.ProfileName)
}

func (l *Loader) parse(ctx context.Context, load bool) error {
	bounded, cancel := context.WithTimeout(ctx, l.cfg.ParserTimeout)
	defer cancel()
	err := l.parser(bounded, append([]byte(nil), l.policy...), load)
	if bounded.Err() != nil {
		return bounded.Err()
	}
	return err
}

func (l *Loader) enforced() (bool, error) {
	contents, err := os.ReadFile(l.cfg.ProfilesPath)
	if err != nil {
		return false, errors.New("cannot read AppArmor securityfs profiles")
	}
	count := 0
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == l.cfg.ProfileName || strings.HasPrefix(line, l.cfg.ProfileName+" ") {
			count++
			if line != l.cfg.ProfileName+" (enforce)" {
				return false, errors.New("expected profile is not in enforce mode")
			}
		}
	}
	if count > 1 {
		return false, errors.New("duplicate exact profile entries")
	}
	return count == 1, nil
}

func (l *Loader) clearReady() error {
	err := os.Remove(l.cfg.ReadinessFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot invalidate readiness")
	}
	return nil
}

// Run retries failed checks while NotReady. Shutdown never unloads kernel policy.
func (l *Loader) Run(ctx context.Context) error {
	defer func() { l.mu.Lock(); defer l.mu.Unlock(); _ = l.clearReady() }()
	timer := time.NewTicker(l.cfg.CheckInterval)
	defer timer.Stop()
	for {
		if err := l.Check(ctx); err != nil && ctx.Err() == nil {
			log.Printf("apparmor-loader verification failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

package redisbootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestPrepareMountRootOnlyChangesRootAndEmptyMetadata(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "redis.conf")
	if err := os.WriteFile(config, []byte("retained userdata"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareMountRoot(context.Background(), dir, os.Geteuid(), os.Getegid(), 0700, true); err != nil {
		t.Fatal("valid owned mount was not prepared", err)
	}
	after, err := os.Stat(config)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("prep changed retained config", err)
	}
	data, err := os.ReadFile(config)
	if err != nil || string(data) != "retained userdata" {
		t.Fatal("prep changed data", err)
	}
}

func TestPrepareMountRootNonemptyMetadataAndUnknownOwnerFailClosed(t *testing.T) {
	for _, name := range []string{"nonempty metadata", "unknown owner", "symlink root"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			uid := os.Geteuid()
			gid := os.Getegid()
			config := filepath.Join(dir, "redis.conf")
			if err := os.WriteFile(config, []byte("private retained config"), 0600); err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(config)
			if err != nil {
				t.Fatal(err)
			}
			path := dir
			switch name {
			case "nonempty metadata":
				if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "lost+found", "unknown"), []byte("do not delete"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown owner":
				if uid == 0 {
					t.Skip("root-only unknown UID case uses full Linux fixture")
				}
				uid++
			case "symlink root":
				path = filepath.Join(t.TempDir(), "mount")
				if err := os.Symlink(dir, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareMountRoot(context.Background(), path, uid, gid, 0700, true); err == nil {
				t.Fatal("unsafe mount accepted")
			}
			after, err := os.Stat(config)
			if err != nil || !os.SameFile(original, after) || after.Mode() != 0600 {
				t.Fatal("failed prep modified retained config", err)
			}
		})
	}
}

func TestPreparedFileExactOwnedRetryAndUnknownPreservation(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	data := []byte("bounded fixture executable")
	if err := installPreparedFile(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal("new file install failed", err)
	}
	before, err := os.Lstat(filepath.Join(dir, "redis-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	if err := installPreparedFile(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal("exact interrupted retry failed", err)
	}
	after, err := os.Lstat(filepath.Join(dir, "redis-bootstrap"))
	if err != nil || !os.SameFile(before, after) || after.Mode() != 0555 {
		t.Fatal("retry replaced existing inode", err)
	}
	if err := installPreparedFile(context.Background(), root, "redis-bootstrap", []byte("foreign executable"), 0555, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("foreign existing tool overwritten")
	}
	actual, err := os.ReadFile(filepath.Join(dir, "redis-bootstrap"))
	if err != nil || string(actual) != string(data) {
		t.Fatal("unknown retry changed file", err)
	}
	if err := os.Link(filepath.Join(dir, "redis-bootstrap"), filepath.Join(dir, "unknown-link")); err != nil {
		t.Fatal(err)
	}
	if err := installPreparedFile(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("hardlinked tool accepted")
	}
}

func TestPreparePodRejectsInvalidIdentityAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PreparePod(ctx, PreparePodOptions{Ordinal: 1, UID: 999, GID: 999}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated", err)
	}
	for _, o := range []PreparePodOptions{{Ordinal: 3, UID: 999, GID: 999}, {Ordinal: 1, UID: 0, GID: 999}, {Ordinal: 1, UID: 999, GID: 0}} {
		if err := PreparePod(context.Background(), o); err == nil {
			t.Fatal("unsafe identity accepted")
		}
	}
	if os.Geteuid() != 0 {
		if err := PreparePod(context.Background(), PreparePodOptions{Ordinal: 1, UID: 999, GID: 999}); err == nil {
			t.Fatal("nonroot public preparation accepted")
		}
	}
}

func rootPreparationFixture(t *testing.T) (podPreparationPaths, PreparePodOptions, []byte) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires actual Linux root/chown fixture; local nonroot tests do not prove ownership")
	}
	base := t.TempDir()
	p := podPreparationPaths{data: filepath.Join(base, "data"), private: filepath.Join(base, "private"), tools: filepath.Join(base, "tools"), seed: filepath.Join(base, "source", "seed"), public: filepath.Join(base, "public.json"), executable: filepath.Join(base, "redis-bootstrap")}
	for _, dir := range []string{p.data, p.private, p.tools, filepath.Dir(p.seed)} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	secret, err := GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{p.seed, secret.Data["redis-sentinel-1"], 0400}, {p.public, secret.Data["public-keys.json"], 0444}, {p.executable, []byte("image owned executable fixture"), 0555}} {
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	return p, PreparePodOptions{Ordinal: 1, UID: 999, GID: 999}, secret.Data["redis-sentinel-1"]
}

func TestPreparePodLinuxRootFixedMountOwnershipAndInterruptedRetry(t *testing.T) {
	p, o, seed := rootPreparationFixture(t)
	config := filepath.Join(p.data, "redis.conf")
	if err := os.WriteFile(config, []byte("retained private data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(config, 999, 999); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(p.data, "lost+found"), 0700); err != nil {
		t.Fatal(err)
	}
	// A prior run fully installed the owned seed, then was interrupted before
	// copying the tool. Retry may retain only that exact completed inode.
	seedPath := filepath.Join(p.private, "seed")
	if err := os.WriteFile(seedPath, seed, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(seedPath, 999, 999); err != nil {
		t.Fatal(err)
	}
	seedBefore, err := os.Lstat(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.Lstat(config)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := preparePod(context.Background(), o, p); err != nil {
			t.Fatal("actual root preparation failed", err)
		}
	}
	for _, f := range []struct {
		path     string
		uid, gid int
		mode     os.FileMode
	}{{p.data, 999, 999, 0700}, {filepath.Join(p.data, "lost+found"), 999, 999, 0700}, {p.private, 999, 999, 0700}, {seedPath, 999, 999, 0400}, {p.tools, 0, 0, 0555}, {filepath.Join(p.tools, "redis-bootstrap"), 0, 0, 0555}} {
		info, err := os.Lstat(f.path)
		if err != nil || !preparationOwner(info, f.uid, f.gid) || info.Mode().Perm() != f.mode {
			t.Fatal("actual final mount ownership/mode mismatch", f.path, err)
		}
	}
	seedAfter, err := os.Lstat(seedPath)
	if err != nil || !os.SameFile(seedBefore, seedAfter) {
		t.Fatal("exact seed retry replaced inode", err)
	}
	configAfter, err := os.Lstat(config)
	if err != nil || !samePreparationFile(configBefore, configAfter) {
		t.Fatal("root prep mutated retained config", err)
	}
}

func TestPreparePodLinuxRootUnsafeMountOrSeedDoesNotInstall(t *testing.T) {
	for _, name := range []string{"unknown UID", "nonempty lostfound", "wrong ordinal seed", "public seed", "hardlink seed", "partial existing seed", "unknown tool entry"} {
		t.Run(name, func(t *testing.T) {
			p, o, seed := rootPreparationFixture(t)
			config := filepath.Join(p.data, "sentinel.conf")
			if err := os.WriteFile(config, []byte("retained private state"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(config)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unknown UID":
				if err := os.Chown(p.data, 4242, 4242); err != nil {
					t.Fatal(err)
				}
			case "nonempty lostfound":
				if err := os.Mkdir(filepath.Join(p.data, "lost+found"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p.data, "lost+found", "unknown"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong ordinal seed":
				o.Ordinal = 2
			case "public seed":
				if err := os.Chmod(p.seed, 0444); err != nil {
					t.Fatal(err)
				}
			case "hardlink seed":
				if err := os.Link(p.seed, filepath.Join(filepath.Dir(p.seed), "link")); err != nil {
					t.Fatal(err)
				}
			case "partial existing seed":
				if err := os.WriteFile(filepath.Join(p.private, "seed"), seed[:3], 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown tool entry":
				if err := os.WriteFile(filepath.Join(p.tools, "unknown"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := preparePod(context.Background(), o, p); err == nil {
				t.Fatal("unsafe root preparation succeeded")
			}
			after, err := os.Lstat(config)
			if err != nil || !samePreparationFile(before, after) {
				t.Fatal("failure changed retained private config", err)
			}
			if _, err := os.Lstat(filepath.Join(p.tools, "redis-bootstrap")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed prep installed tooling", err)
			}
			if name != "partial existing seed" {
				if _, err := os.Lstat(filepath.Join(p.private, "seed")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unsafe prep installed member seed", err)
				}
			}
		})
	}
}

func TestPreparationRejectsNonregularSourceWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparationSource(context.Background(), path, 32, 0400, false); err == nil {
		t.Fatal("FIFO accepted as private source")
	}
}

func TestPreparedAtomicInterruptionsNeverCreatePartialFinalAndRetry(t *testing.T) {
	for _, point := range []string{"prewrite", "afterwrite", "beforechown", "afterSync", "beforelink", "afterlink", "beforeunlink"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			data := []byte("trusted bounded tooling bytes")
			triggered := false
			hook := func(at string) error {
				if at == point {
					triggered = true
					return errors.New("interrupted prepared copy")
				}
				return nil
			}
			if err := installPreparedFileWithHook(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid(), hook); !triggered || err == nil {
				t.Fatal("copy interruption checkpoint missing", err)
			}
			path := filepath.Join(dir, "redis-bootstrap")
			if info, err := os.Lstat(path); err == nil {
				actual, err := os.ReadFile(path)
				if err != nil || string(actual) != string(data) || info.Mode() != 0555 {
					t.Fatal("copy created partial/unprotected final", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := installPreparedFile(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid()); err != nil {
				t.Fatal("normal interruption stranded prepare retry", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, ".prepare-redis-bootstrap")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed retry left owned temp", err)
			}
		})
	}
}

func TestPreparedAtomicRecoversOnlyExactOwnedTempOrPair(t *testing.T) {
	for _, name := range []string{"partial prefix", "complete temp", "two link pair", "unknown bytes", "third link", "symlink", "partial final", "full legacy final"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			data := []byte("trusted exact image tooling source")
			temp := filepath.Join(dir, ".prepare-redis-bootstrap")
			final := filepath.Join(dir, "redis-bootstrap")
			input := data
			mode := os.FileMode(0555)
			good := true
			switch name {
			case "partial prefix":
				input = data[:4]
				mode = 0600
			case "unknown bytes":
				input = []byte("foreign temp bytes")
				good = false
			case "third link", "symlink", "partial final":
				good = false
			case "full legacy final":
				mode = 0600
			}
			p := temp
			if name == "partial final" {
				p = final
				input = data[:4]
				mode = 0600
			}
			if name == "full legacy final" {
				p = final
			}
			if name == "symlink" {
				if err := os.WriteFile(filepath.Join(dir, "other"), data, 0555); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "other"), temp); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(p, input, mode); err != nil {
					t.Fatal(err)
				}
			}
			if name == "two link pair" || name == "third link" {
				if err := os.Link(temp, final); err != nil {
					t.Fatal(err)
				}
			}
			if name == "third link" {
				if err := os.Link(temp, filepath.Join(dir, "foreign-link")); err != nil {
					t.Fatal(err)
				}
			}
			err = installPreparedFile(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid())
			if good && err != nil {
				t.Fatal("known prepared state failed safe retry", err)
			}
			if !good && err == nil {
				t.Fatal("unknown prepared state adopted")
			}
			if !good {
				actual, err := os.Lstat(p)
				if err != nil || actual == nil {
					t.Fatal("failure removed unknown existing state", err)
				}
			}
		})
	}
}

func TestPreparedAtomicPreservesUnknownReplacementAtCheckpoints(t *testing.T) {
	for _, point := range []string{"prewrite", "afterwrite", "beforechown", "afterSync", "beforelink", "afterlink", "beforeunlink"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			data := []byte("trusted immutable image tooling")
			unknown := filepath.Join(dir, ".prepare-redis-bootstrap")
			changed := false
			hook := func(at string) error {
				if at != point {
					return nil
				}
				changed = true
				p := filepath.Join(dir, "replacement")
				if err := os.WriteFile(p, []byte("unknown replacement"), 0600); err != nil {
					return err
				}
				return os.Rename(p, unknown)
			}
			if err := installPreparedFileWithHook(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid(), hook); !changed || err == nil {
				t.Fatal("unknown temp replacement authorized install", err)
			}
			b, err := os.ReadFile(unknown)
			if err != nil || !strings.Contains(string(b), "unknown replacement") {
				t.Fatal("cleanup removed unknown replacement", err)
			}
		})
	}
}

func TestPreparePodLinuxRootAtomicSeedAndToolInterruptions(t *testing.T) {
	for _, name := range []string{"seed", "redis-bootstrap"} {
		for _, point := range []string{"prewrite", "afterwrite", "beforechown", "afterSync", "beforelink", "afterlink", "beforeunlink"} {
			t.Run(name+"/"+point, func(t *testing.T) {
				p, o, seed := rootPreparationFixture(t)
				if err := prepareMountRoot(context.Background(), p.private, 999, 999, 0700, false); err != nil {
					t.Fatal(err)
				}
				if err := prepareMountRoot(context.Background(), p.tools, 0, 0, 0555, false); err != nil {
					t.Fatal(err)
				}
				dir, data, mode, uid, gid := p.private, seed, os.FileMode(0400), 999, 999
				if name == "redis-bootstrap" {
					dir = p.tools
					var err error
					data, err = os.ReadFile(p.executable)
					if err != nil {
						t.Fatal(err)
					}
					mode = 0555
					uid = 0
					gid = 0
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				triggered := false
				hook := func(at string) error {
					if at == point {
						triggered = true
						return errors.New("root init interrupted")
					}
					return nil
				}
				copyErr := installPreparedFileWithHook(context.Background(), root, name, data, mode, uid, gid, hook)
				closeErr := root.Close()
				if !triggered || copyErr == nil || closeErr != nil {
					t.Fatal("actual root copy interruption not exercised", copyErr, closeErr)
				}
				if info, err := os.Lstat(filepath.Join(dir, name)); err == nil {
					actual, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil || string(actual) != string(data) || info.Mode() != mode || !preparationOwner(info, uid, gid) {
						t.Fatal("actual root interruption exposed partial/unprotected final", err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := preparePod(context.Background(), o, p); err != nil {
					t.Fatal("actual root ordinary interruption stranded init", err)
				}
				if _, err := os.Lstat(filepath.Join(dir, ".prepare-"+name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("actual root retry did not repair temp/pair", err)
				}
			})
		}
	}
}

func TestPreparePodLinuxRootLegacySeedFinalExactOnly(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0400} {
		t.Run(mode.String(), func(t *testing.T) {
			p, o, seed := rootPreparationFixture(t)
			path := filepath.Join(p.private, "seed")
			if err := os.WriteFile(path, seed, mode); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := preparePod(context.Background(), o, p); err != nil {
				t.Fatal("root source-exact legacy final could not finish metadata", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || after.Mode() != 0400 || !preparationOwner(after, 999, 999) {
				t.Fatal("legacy final not completed on same original inode", err)
			}
		})
	}
}

func TestPreparedAtomicPreservesSourceEquivalentReplacementInode(t *testing.T) {
	for _, point := range []string{"afterSync", "beforelink", "afterlink", "beforeunlink"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			data := []byte("trusted unchanged source bytes")
			path := filepath.Join(dir, ".prepare-redis-bootstrap")
			var replacement os.FileInfo
			hook := func(at string) error {
				if at != point {
					return nil
				}
				p := filepath.Join(dir, "replacement")
				if err := os.WriteFile(p, data, 0555); err != nil {
					return err
				}
				if err := os.Rename(p, path); err != nil {
					return err
				}
				var err error
				replacement, err = os.Lstat(path)
				return err
			}
			if err := installPreparedFileWithHook(context.Background(), root, "redis-bootstrap", data, 0555, os.Geteuid(), os.Getegid(), hook); err == nil || replacement == nil {
				t.Fatal("source-equivalent unknown inode adopted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(replacement, after) {
				t.Fatal("cleanup removed source-equivalent unknown replacement", err)
			}
		})
	}
}

func TestPreparePodLinuxRootAtomicUnknownSeedInodePreserved(t *testing.T) {
	for _, point := range []string{"prewrite", "afterwrite", "beforechown", "afterSync", "beforelink", "afterlink", "beforeunlink"} {
		t.Run(point, func(t *testing.T) {
			p, _, seed := rootPreparationFixture(t)
			root, err := os.OpenRoot(p.private)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			path := filepath.Join(p.private, ".prepare-seed")
			var replacement os.FileInfo
			hook := func(at string) error {
				if at != point {
					return nil
				}
				p := filepath.Join(p.private, "replacement")
				if err := os.WriteFile(p, seed, 0400); err != nil {
					return err
				}
				if err := os.Chown(p, 999, 999); err != nil {
					return err
				}
				if err := os.Rename(p, path); err != nil {
					return err
				}
				var err error
				replacement, err = os.Lstat(path)
				return err
			}
			if err := installPreparedFileWithHook(context.Background(), root, "seed", seed, 0400, 999, 999, hook); err == nil || replacement == nil {
				t.Fatal("actual root adopted unknown source-equivalent seed inode")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(replacement, after) {
				t.Fatal("actual root cleanup removed unknown inode", err)
			}
		})
	}
}

//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func protectedParent(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(p, 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

// Removing no-follow, exact owner/mode, or single-link checks admits these
// actual unsafe filesystem objects, rather than a synthetic stat result.
func TestJournalProtectedFiles(t *testing.T) {
	t.Run("canonical-parent", func(t *testing.T) {
		p := protectedParent(t)
		f, err := openJournalParent(filepath.Join(p, "journal"), uint32(os.Geteuid()))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		link := filepath.Join(p, "alias")
		if err = os.Symlink(p, link); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(link, "journal"), p + "/../" + filepath.Base(p) + "/journal", "relative/journal"} {
			if f, err = openJournalParent(path, uint32(os.Geteuid())); err == nil {
				f.Close()
				t.Fatalf("unsafe parent accepted: %s", path)
			}
		}
		if err = os.Chmod(p, 0750); err != nil {
			t.Fatal(err)
		}
		if f, err = openJournalParent(filepath.Join(p, "journal"), uint32(os.Geteuid())); err == nil {
			f.Close()
			t.Fatal("unsafe parent mode accepted")
		}
	})
	t.Run("child-protection", func(t *testing.T) {
		p := protectedParent(t)
		parent, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		fs := journalFiles{uid: uint32(os.Geteuid())}
		if err = os.WriteFile(filepath.Join(p, "file"), []byte("safe"), 0600); err != nil {
			t.Fatal(err)
		}
		b, err := fs.readFile(context.Background(), parent, "file")
		if err != nil || string(b) != "safe" {
			t.Fatalf("read %q: %v", b, err)
		}
		cases := []struct {
			name  string
			setup func() error
		}{
			{"symlink", func() error { return os.Symlink("file", filepath.Join(p, "bad")) }},
			{"hardlink", func() error { return os.Link(filepath.Join(p, "file"), filepath.Join(p, "bad")) }},
			{"unsafe-mode", func() error { return os.WriteFile(filepath.Join(p, "bad"), nil, 0640) }},
			{"directory", func() error { return os.Mkdir(filepath.Join(p, "bad"), 0700) }},
			{"fifo", func() error { return unix.Mkfifo(filepath.Join(p, "bad"), 0600) }},
			{"oversize", func() error { return os.WriteFile(filepath.Join(p, "bad"), make([]byte, 8193), 0600) }},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				if err := tt.setup(); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(filepath.Join(p, "bad"))
				if _, err := fs.readFile(context.Background(), parent, "bad"); err == nil {
					t.Fatal("unsafe child accepted")
				}
			})
		}
		for _, name := range []string{"../file", "/file", "a/b", ".", ".."} {
			if _, err := fs.readFile(context.Background(), parent, name); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("invalid name %q: %v", name, err)
			}
		}
	})
	t.Run("actual-wrong-owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("requires UID0 for actual chown; controller Linux fixture runs this")
		}
		p := protectedParent(t)
		parent, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		fs := journalFiles{uid: 0}
		path := filepath.Join(p, "file")
		if err = os.WriteFile(path, []byte("owner"), 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.Chown(path, 12345, -1); err != nil {
			t.Fatal(err)
		}
		if _, err = fs.readFile(context.Background(), parent, "file"); err == nil {
			t.Fatal("wrong file owner accepted")
		}
		if err = os.Chown(p, 12345, -1); err != nil {
			t.Fatal(err)
		}
		defer os.Chown(p, 0, -1)
		if f, err := openJournalParent(filepath.Join(p, "journal"), 0); err == nil {
			f.Close()
			t.Fatal("wrong parent owner accepted")
		}
	})
}

func TestJournalProtectedFilesLayout(t *testing.T) {
	for _, part := range []string{"root", "commands", "bucket", "gate", "lock", "record"} {
		t.Run(part+"-symlink", func(t *testing.T) {
			j, o := createJournalFixture(t)
			j.Close()
			writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
			path := protectedLayoutPath(o, part)
			saved := path + ".saved"
			if err := os.Rename(path, saved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(saved, path); err != nil {
				t.Fatal(err)
			}
			if j, err := OpenClosedJournal(context.Background(), o); err == nil {
				j.Close()
				t.Fatal("symlink accepted")
			}
		})
		t.Run(part+"-unsafe-mode", func(t *testing.T) {
			j, o := createJournalFixture(t)
			j.Close()
			writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
			path := protectedLayoutPath(o, part)
			if err := os.Chmod(path, 0770); err != nil {
				t.Fatal(err)
			}
			if j, err := OpenClosedJournal(context.Background(), o); err == nil {
				j.Close()
				t.Fatal("unsafe mode accepted")
			}
		})
	}
	for _, part := range []string{"gate", "lock", "record"} {
		t.Run(part+"-hardlink", func(t *testing.T) {
			j, o := createJournalFixture(t)
			j.Close()
			writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
			path := protectedLayoutPath(o, part)
			if err := os.Link(path, filepath.Join(filepath.Dir(o.Directory), "otherlink")); err != nil {
				t.Fatal(err)
			}
			if j, err := OpenClosedJournal(context.Background(), o); err == nil {
				j.Close()
				t.Fatal("hardlinked journal accepted")
			}
		})
	}
	t.Run("actual-layout-wrong-owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("requires UID0 for actual chown; controller Linux fixture runs this")
		}
		for _, part := range []string{"root", "commands", "bucket", "gate", "lock", "record"} {
			t.Run(part, func(t *testing.T) {
				j, o := createJournalFixture(t)
				j.Close()
				writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
				path := protectedLayoutPath(o, part)
				if err := os.Chown(path, 12345, -1); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.Chown(path, 0, -1); err != nil {
						t.Errorf("restore owner: %v", err)
					}
				}()
				if j, err := OpenClosedJournal(context.Background(), o); err == nil {
					j.Close()
					t.Fatal("wrong owner accepted")
				}
			})
		}
	})
}
func protectedLayoutPath(o JournalOptions, part string) string {
	switch part {
	case "root":
		return o.Directory
	case "commands":
		return filepath.Join(o.Directory, "commands")
	case "bucket":
		return filepath.Join(o.Directory, "commands", "22")
	case "gate":
		return filepath.Join(o.Directory, "gate.json")
	case "lock":
		return filepath.Join(o.Directory, "lock")
	default:
		return filepath.Join(o.Directory, "commands", "22", "22222222-2222-4222-8222-222222222222.json")
	}
}

func TestJournalProtectedFilesPinnedRoot(t *testing.T) {
	j, o := createJournalFixture(t)
	moved := o.Directory + "-moved"
	if err := os.Rename(o.Directory, moved); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(filepath.Dir(o.Directory), "decoy")
	if err := os.Mkdir(decoy, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(decoy, "gate.json"), []byte("decoy must remain untouched"))
	if err := os.Symlink(decoy, o.Directory); err != nil {
		t.Fatal(err)
	}
	if _, err := j.CloseGate(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(decoy, "gate.json"))
	if err != nil || string(b) != "decoy must remain untouched" {
		t.Fatalf("followed replaced root %s %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(moved, "gate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var gate GateManifest
	if err = decodeGateManifest(b, &gate); err != nil || gate.GateState != "closed" {
		t.Fatal("pinned root not closed", err)
	}
}

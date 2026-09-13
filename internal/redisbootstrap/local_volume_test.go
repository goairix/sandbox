package redisbootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestLocalVolumeRejectsUnsafeRetainedFiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string)
	}{
		{"special mode bits", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Chmod(filepath.Join(dir, "redis.conf"), 0600|os.ModeSetuid); err != nil {
				t.Fatal(err)
			}
		}},
		{"public mode", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Chmod(filepath.Join(dir, "redis.conf"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing config", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Remove(filepath.Join(dir, "sentinel.conf")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hard link", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Link(filepath.Join(dir, "identity.json"), filepath.Join(dir, "alias")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Rename(filepath.Join(dir, "redis.conf"), filepath.Join(dir, "secret-do-not-log")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("secret-do-not-log", filepath.Join(dir, "redis.conf")); err != nil {
				t.Fatal(err)
			}
		}},
		{"nonregular", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Remove(filepath.Join(dir, "redis.conf")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "redis.conf"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "redis.conf"), []byte(strings.Repeat("#", maximumPersistentConfigBytes+1)), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty config", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "redis.conf"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, c, m := configuredLocalFixture(t)
			tc.change(t, dir)
			got, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
			if err == nil || got.Volume.Identity != nil || got.Snapshot != nil || got.ConfigDigest != "" {
				t.Fatalf("accepted unsafe file: %+v", got)
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "secret-do-not-log") {
				t.Fatal("path leaked")
			}
		})
	}
}

type localInfoWithStat struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i localInfoWithStat) Sys() any { return &i.stat }

func TestLocalVolumeRejectsForeignOwnerMetadata(t *testing.T) {
	dir, _, _ := configuredLocalFixture(t)
	info, err := os.Stat(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("unsupported stat evidence")
	}
	foreign := *stat
	foreign.Uid++
	if privateLocalInfo(localInfoWithStat{FileInfo: info, stat: foreign}, maximumStateBytes) {
		t.Fatal("accepted foreign owner")
	}
}

func TestLocalVolumeRejectsChangedByteAndInodeEvidence(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	root, err := openLocalRoot(context.Background(), dir, c, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	data, info, err := readPrivateLocalFile(context.Background(), root, "redis.conf", maximumPersistentConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := unchangedLocalFile(context.Background(), root, "redis.conf", maximumPersistentConfigBytes, data, info); err != nil {
		t.Fatal(err)
	}
	changedTime := info.ModTime().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "redis.conf"), changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	if err := unchangedLocalFile(context.Background(), root, "redis.conf", maximumPersistentConfigBytes, data, info); err == nil {
		t.Fatal("accepted changed mtime")
	}
	changed := append([]byte(nil), data...)
	changed[0] = '#'
	if err := os.WriteFile(filepath.Join(dir, "redis.conf"), changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "redis.conf"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := unchangedLocalFile(context.Background(), root, "redis.conf", maximumPersistentConfigBytes, data, info); err == nil {
		t.Fatal("accepted changed bytes")
	}
	if err := os.WriteFile(filepath.Join(dir, "replacement"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "replacement"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "redis.conf")); err != nil {
		t.Fatal(err)
	}
	if err := unchangedLocalFile(context.Background(), root, "redis.conf", maximumPersistentConfigBytes, data, info); err == nil {
		t.Fatal("accepted same bytes replacement inode")
	}
}

func TestLocalVolumeIdentityAndRootBounds(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	identityPath := filepath.Join(dir, "identity.json")
	data, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	exact := append(data, []byte(strings.Repeat(" ", maximumStateBytes-len(data)))...)
	if err := os.WriteFile(identityPath, exact, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalVolume(context.Background(), dir, c, m, "main"); err != nil {
		t.Fatalf("exact identity bound: %v", err)
	}
	if err := os.WriteFile(identityPath, append(exact, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalVolume(context.Background(), dir, c, m, "main"); err == nil {
		t.Fatal("accepted oversized identity")
	}
	if err := os.WriteFile(identityPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 125; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("temp-data-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadLocalVolume(context.Background(), dir, c, m, "main"); err != nil {
		t.Fatalf("128 entry bound: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "temp-data-extra"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalVolume(context.Background(), dir, c, m, "main"); err == nil {
		t.Fatal("accepted oversized root inventory")
	}
}

func TestLocalVolumeRejectsExternalAndMetadataSymlinks(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private"), []byte("port 6379\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "redis.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "private"), filepath.Join(dir, "redis.conf")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalVolume(context.Background(), dir, c, m, "main"); err == nil {
		t.Fatal("accepted outside symlink")
	}
	fresh := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(fresh, "lost+found")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalVolume(context.Background(), fresh, c, m, "main"); err == nil {
		t.Fatal("accepted lost+found symlink")
	}
	rootAlias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(outside, rootAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalVolume(context.Background(), rootAlias, c, m, "main"); err == nil {
		t.Fatal("accepted root symlink")
	}
}

func TestLocalVolumeNeverReseedsOldOrPartialState(t *testing.T) {
	c := testCluster()
	m := testMember(c, 0)
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"old data", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "dump.rdb"), []byte("old-data"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown directory", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, "unknown"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"nonempty metadata", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "lost+found", "old"), []byte("old-data"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"partial reservation", func(t *testing.T, dir string) {
			t.Helper()
			id, err := NewVolumeIdentity(c, m)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteVolumeIdentity(context.Background(), filepath.Join(dir, "identity.json"), id, c); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "redis.conf"), []byte("port 6379\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			if _, err := ReserveLocalVolume(context.Background(), dir, c, m, "main"); err == nil {
				t.Fatal("reseeded old or partial volume")
			}
		})
	}
}

func TestLocalVolumeMetadataMembershipAndCancellation(t *testing.T) {
	c := testCluster()
	m := testMember(c, 0)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || !got.Volume.Empty {
		t.Fatalf("metadata: %+v, %v", got, err)
	}
	id, err := ReserveLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil {
		t.Fatal(err)
	}
	wrong := c
	wrong.ClusterID = "different-cluster"
	if _, err := ReadLocalVolume(context.Background(), dir, wrong, m, "main"); err == nil {
		t.Fatal("accepted foreign cluster")
	}
	if _, err := ReadLocalVolume(context.Background(), dir, c, testMember(c, 1), "main"); err == nil {
		t.Fatal("accepted foreign member")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadLocalVolume(ctx, dir, c, m, "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	got, err = ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || *got.Volume.Identity != id {
		t.Fatal("membership check rewrote marker")
	}
	for _, path := range []string{"", "/", "relative"} {
		if _, err := ReadLocalVolume(context.Background(), path, c, m, "main"); err == nil {
			t.Fatal("accepted broad/relative root")
		}
	}
}

func TestLocalVolumeDigestTracksActualRetainedConfiguration(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	before, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil {
		t.Fatal(err)
	}
	r, s := persistentFixture(c, 2, 2)
	s = strings.ReplaceAll(s, "current-epoch 11", "current-epoch 12")
	if err := os.WriteFile(filepath.Join(dir, "redis.conf"), []byte(r), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sentinel.conf"), []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || after.ConfigDigest == before.ConfigDigest || after.Snapshot.CurrentEpoch != 12 || after.Snapshot.State.SentinelEpoch != 7 {
		t.Fatalf("digest didn't track actual config: %v", err)
	}
}

func TestLocalVolumeFreshAndIdempotentReservation(t *testing.T) {
	c := testCluster()
	m := testMember(c, 1)
	dir := t.TempDir()
	got, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || !got.Volume.Empty {
		t.Fatalf("fresh: %+v, %v", got, err)
	}
	id, err := ReserveLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || id.InitialConfig != Reserved {
		t.Fatalf("reserve: %+v, %v", id, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := ReserveLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || id2 != id {
		t.Fatalf("repeat changed marker: %+v, %v", id2, err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("identity rewritten")
	}
	info, err := os.Stat(filepath.Join(dir, "identity.json"))
	if err != nil || info.Mode() != 0600 {
		t.Fatal("reservation changed private mode")
	}
	got, err = ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || got.Volume.Empty || got.Volume.Identity == nil || *got.Volume.Identity != id || got.Volume.Persisted != nil || got.Snapshot != nil || got.ConfigDigest != "" {
		t.Fatalf("reserved: %+v, %v", got, err)
	}
}

func configuredLocalFixture(t *testing.T) (string, ClusterState, Member) {
	t.Helper()
	c := testCluster()
	m := testMember(c, 2)
	dir := t.TempDir()
	id, err := NewVolumeIdentity(c, m)
	if err != nil {
		t.Fatal(err)
	}
	id.InitialConfig = Configured
	if err := WriteVolumeIdentity(context.Background(), filepath.Join(dir, "identity.json"), id, c); err != nil {
		t.Fatal(err)
	}
	r, s := persistentFixture(c, 2, 2)
	for name, value := range map[string]string{"redis.conf": r, "sentinel.conf": s} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, c, m
}

func TestLocalVolumeConfiguredNonzeroPrimary(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	before := make(map[string][]byte)
	for _, name := range []string{"identity.json", "redis.conf", "sentinel.conf"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	got, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || got.Snapshot == nil || got.Volume.Persisted == nil || got.Volume.Persisted.Role != Primary || got.Volume.Persisted.PrimaryDNS != m.DNS || len(got.ConfigDigest) != 64 {
		t.Fatalf("configured: %+v, %v", got, err)
	}
	id, err := ReserveLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || id != *got.Volume.Identity {
		t.Fatalf("configured repeat: %+v, %v", id, err)
	}
	for name, data := range before {
		after, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != string(after) {
			t.Fatal("configured reservation rewrote files")
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode() != 0600 {
			t.Fatal("configured reservation changed private mode")
		}
	}
}

func TestLocalVolumeConcurrentReservation(t *testing.T) {
	c := testCluster()
	m := testMember(c, 0)
	dir := t.TempDir()
	var wg sync.WaitGroup
	ids := make(chan VolumeIdentity, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			id, err := ReserveLocalVolume(context.Background(), dir, c, m, "main")
			if err != nil {
				errs <- err
			} else {
				ids <- id
			}
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	var first VolumeIdentity
	for id := range ids {
		if first.MarkerID == "" {
			first = id
		}
		if id != first {
			t.Fatal("two markers")
		}
	}
	// Concurrent readers may observe the exclusive-create temporary hard link;
	// they may retry, but must never replace the winner's marker.
	if first.MarkerID == "" {
		t.Fatalf("no successful reservation: %v", <-errs)
	}
	final, err := ReserveLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil || final != first {
		t.Fatalf("winner changed: %+v, %v", final, err)
	}
}

func TestLocalVolumeReservationPreservesUnconfirmedDurability(t *testing.T) {
	c := testCluster()
	m := testMember(c, 0)
	dir := t.TempDir()
	writer := func(ctx context.Context, path string, id VolumeIdentity, cluster ClusterState) error {
		if err := WriteVolumeIdentity(ctx, path, id, cluster); err != nil {
			return err
		}
		// Model the low-level installed marker whose final directory sync failed.
		return &os.PathError{Op: "sync", Path: path, Err: syscall.EIO}
	}
	if _, err := reserveLocalVolume(context.Background(), dir, c, m, "main", writer, confirmLocalReservation); err == nil {
		t.Fatal("laundered unconfirmed durability into success")
	}
}

func TestLocalVolumeReservationRechecksUnexpectedData(t *testing.T) {
	c := testCluster()
	m := testMember(c, 0)
	dir := t.TempDir()
	writer := func(ctx context.Context, path string, id VolumeIdentity, cluster ClusterState) error {
		if err := WriteVolumeIdentity(ctx, path, id, cluster); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "dump.rdb"), []byte("unexpected-data"), 0600)
	}
	if _, err := reserveLocalVolume(context.Background(), dir, c, m, "main", writer, confirmLocalReservation); err == nil {
		t.Fatal("confirmed reservation after data appeared")
	}
}

func TestLocalVolumeReservationConfirmsExclusiveCreateWinner(t *testing.T) {
	c := testCluster()
	m := testMember(c, 0)
	dir := t.TempDir()
	writer := func(ctx context.Context, path string, id VolumeIdentity, cluster ClusterState) error {
		if err := WriteVolumeIdentity(ctx, path, id, cluster); err != nil {
			return err
		}
		return &os.LinkError{Op: "link", Old: "temporary", New: path, Err: os.ErrExist}
	}
	id, err := reserveLocalVolume(context.Background(), dir, c, m, "main", writer, confirmLocalReservation)
	if err != nil || id.InitialConfig != Reserved {
		t.Fatalf("exclusive winner: %+v, %v", id, err)
	}
}

func TestLocalVolumeExistingReservationRequiresDurabilityConfirmation(t *testing.T) {
	c := testCluster()
	m := testMember(c, 0)
	dir := t.TempDir()
	id, err := NewVolumeIdentity(c, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteVolumeIdentity(context.Background(), filepath.Join(dir, "identity.json"), id, c); err != nil {
		t.Fatal(err)
	}
	called := false
	confirmer := func(context.Context, string, ClusterState, Member, string, VolumeIdentity) error {
		called = true
		return syscall.EIO
	}
	writer := func(context.Context, string, VolumeIdentity, ClusterState) error {
		t.Fatal("existing marker rewritten")
		return nil
	}
	if _, err := reserveLocalVolume(context.Background(), dir, c, m, "main", writer, confirmer); err == nil || !called {
		t.Fatal("existing marker bypassed failed durability confirmation")
	}
}

func TestBuiltinSentinelPassword(t *testing.T) {
	for _, good := range []string{strings.Repeat("a", 32), strings.Repeat("Z0_-", 64)} {
		if err := ValidateBuiltinSentinelPassword(good); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 257), strings.Repeat("a", 32) + " ", strings.Repeat("a", 32) + "\"", strings.Repeat("a", 32) + "中", strings.Repeat("a", 32) + "\n"} {
		if err := ValidateBuiltinSentinelPassword(bad); err == nil {
			t.Fatal("accepted unsafe token")
		}
	}
}

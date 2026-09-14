package redisbootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
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

func initialTransactionFixture(t *testing.T, ordinal int) (string, BootstrapRegistration, [3]ed25519.PublicKey, Member, InitialConfigOptions) {
	t.Helper()
	f := newRegistrationFixture(t)
	var directories [3]string
	for i := range directories {
		directories[i] = t.TempDir()
		identity := *f.proofs[i].Observation.Volume.Identity
		if err := WriteVolumeIdentity(context.Background(), filepath.Join(directories[i], "identity.json"), identity, f.cluster); err != nil {
			t.Fatal(err)
		}
		observation, err := ReadLocalVolume(context.Background(), directories[i], f.cluster, testMember(f.cluster, i), "sandbox")
		if err != nil {
			t.Fatal(err)
		}
		f.proofs[i] = f.sign(t, i, f.challenges[i], LocalObservation{Volume: observation.Volume})
	}
	r, err := RegisterFreshVolumes(f.cluster, f.keys, f.challenges, f.proofs)
	if err != nil {
		t.Fatal(err)
	}
	options := InitialConfigOptions{MasterName: "sandbox", DataPassword: strings.Repeat("d", 40), SentinelPassword: strings.Repeat("s", 40), DownAfterMilliseconds: 10000, FailoverTimeoutMilliseconds: 60000, ParallelSyncs: 1}
	return directories[ordinal], r, f.keys, testMember(f.cluster, ordinal), options
}

func TestInitialTransactionConfiguredChangedInodeDuringSyncRejected(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 1)
	if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err != nil {
		t.Fatal(err)
	}
	changed := false
	hook := func(point string) error {
		if point == "sync-file:sentinel.conf" && !changed {
			changed = true
			data, err := os.ReadFile(filepath.Join(directory, "identity.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "replacement"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(directory, "replacement"), filepath.Join(directory, "identity.json")); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, hook)
	if !changed || err == nil || out.Volume.Identity != nil {
		t.Fatal("same-byte changed inode escaped transaction stability check")
	}
}

func TestInitialTransactionRejectsDataAppearingBeforePhase(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 0)
	changed := false
	out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
		if point == "before-phase" {
			changed = true
			if err := os.WriteFile(filepath.Join(directory, "dump.rdb"), []byte("unapproved old data"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if !changed || err == nil || out.Volume.Identity != nil {
		t.Fatal("new data appeared during Reserved transaction but configuration was authorized")
	}
	data, err := os.ReadFile(filepath.Join(directory, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ParseVolumeIdentity(data, r.Cluster)
	if err != nil || identity.InitialConfig != Reserved {
		t.Fatal("reservation phase advanced after unapproved data appeared")
	}
}

func TestInitialTransactionReservedRevalidatesSyncedFilesBeforePhase(t *testing.T) {
	for _, target := range []string{"initial-config.json", "redis.conf", "sentinel.conf"} {
		for _, change := range []string{"changed-bytes", "same-bytes-new-inode"} {
			for _, checkpoint := range []string{"before-phase", "sync-temp:identity.json"} {
				t.Run(target+"/"+change+"/"+checkpoint, func(t *testing.T) {
					directory, r, keys, member, options := initialTransactionFixture(t, 0)
					changed := false
					out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
						if point != checkpoint {
							return nil
						}
						changed = true
						path := filepath.Join(directory, target)
						data, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						if change == "changed-bytes" {
							data = append(data, '\n')
						}
						if err := os.WriteFile(filepath.Join(directory, "replacement"), data, 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(filepath.Join(directory, "replacement"), path); err != nil {
							t.Fatal(err)
						}
						return nil
					})
					if !changed || err == nil || out.Volume.Identity != nil || out.Snapshot != nil {
						t.Fatal("unsynced changed retained file advanced initial configuration")
					}
					data, err := os.ReadFile(filepath.Join(directory, "identity.json"))
					if err != nil {
						t.Fatal(err)
					}
					identity, err := ParseVolumeIdentity(data, r.Cluster)
					if err != nil || identity.InitialConfig != Reserved || identity.MarkerID != r.MarkerIDs[0] {
						t.Fatal("phase advanced before synced files were revalidated")
					}
				})
			}
		}
	}
}

func TestInitialTransactionConfiguredManifestStillMustBePrivate(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 2)
	if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "initial-config.json"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options)
	if err == nil || out.Volume.Identity != nil {
		t.Fatal("configured retained secret manifest with unsafe permissions accepted")
	}
}

func TestInitialTransactionRefusesDataAppearingWhilePhaseTempSyncs(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 0)
	changed := false
	out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
		if point == "sync-temp:identity.json" {
			changed = true
			if err := os.WriteFile(filepath.Join(directory, "dump.rdb"), []byte("unapproved data"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if !changed || err == nil || out.Volume.Identity != nil {
		t.Fatal("unapproved data during phase staging advanced configuration")
	}
	data, err := os.ReadFile(filepath.Join(directory, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ParseVolumeIdentity(data, r.Cluster)
	if err != nil || identity.InitialConfig != Reserved {
		t.Fatal("new data advanced phase during staging")
	}
}

func TestInitialTransactionPhaseTempMustBeTheSyncedInode(t *testing.T) {
	for _, change := range []string{"same-bytes-new-inode", "changed-bytes"} {
		t.Run(change, func(t *testing.T) {
			directory, r, keys, member, options := initialTransactionFixture(t, 0)
			changed := false
			changedTemp := ""
			out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
				if point != "sync-temp:identity.json" {
					return nil
				}
				entries, err := os.ReadDir(directory)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".bootstrap-identity-") {
						changed = true
						changedTemp = entry.Name()
						path := filepath.Join(directory, entry.Name())
						data, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						if change == "changed-bytes" {
							data = bytes.ReplaceAll(data, []byte(r.MarkerIDs[0]), []byte(strings.Repeat("f", 32)))
						}
						if err := os.WriteFile(filepath.Join(directory, "replacement"), data, 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(filepath.Join(directory, "replacement"), path); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			})
			if !changed || err == nil || out.Volume.Identity != nil {
				t.Fatal("replacement phase temp inode accepted")
			}
			data, err := os.ReadFile(filepath.Join(directory, "identity.json"))
			if err != nil {
				t.Fatal(err)
			}
			identity, err := ParseVolumeIdentity(data, r.Cluster)
			if err != nil || identity.InitialConfig != Reserved || identity.MarkerID != r.MarkerIDs[0] {
				t.Fatal("unsynced replacement phase temp installed")
			}
			if _, err := os.Lstat(filepath.Join(directory, changedTemp)); err != nil {
				t.Fatal("different-inode temporary replacement was removed as though owned")
			}
		})
	}
}

func TestInitialTransactionPostPhaseNeverAdoptsReplacementFiles(t *testing.T) {
	for _, target := range []string{"identity.json", "initial-config.json", "redis.conf", "sentinel.conf"} {
		for _, change := range []string{"same-bytes-new-inode", "changed-bytes"} {
			t.Run(target+"/"+change, func(t *testing.T) {
				directory, r, keys, member, options := initialTransactionFixture(t, 0)
				changed := false
				out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
					if point != "phase-installed" {
						return nil
					}
					changed = true
					path := filepath.Join(directory, target)
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if change == "changed-bytes" {
						data = append(data, '\n')
					}
					if err := os.WriteFile(filepath.Join(directory, "replacement"), data, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(filepath.Join(directory, "replacement"), path); err != nil {
						t.Fatal(err)
					}
					return nil
				})
				if !changed || err == nil || out.Volume.Identity != nil || out.Snapshot != nil || out.ConfigDigest != "" {
					t.Fatal("post-phase replacement was adopted as a fresh baseline")
				}
				if _, err := os.Lstat(filepath.Join(directory, target)); err != nil {
					t.Fatal("post-phase replacement was removed or rolled back")
				}
			})
		}
	}
}

func TestInitialTransactionRepairsExactOwnedHardlinkInstallWindow(t *testing.T) {
	for _, target := range []string{"initial-config.json", "redis.conf", "sentinel.conf"} {
		t.Run(target, func(t *testing.T) {
			directory, r, keys, member, options := initialTransactionFixture(t, 0)
			stop := "manifest-installed"
			prefix := ".bootstrap-plan-"
			if target == "redis.conf" {
				stop = "redis-installed"
				prefix = ".bootstrap-redis-"
			}
			if target == "sentinel.conf" {
				stop = "sentinel-installed"
				prefix = ".bootstrap-sentinel-"
			}
			if _, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
				if point == stop {
					return errors.New("stop")
				}
				return nil
			}); err == nil {
				t.Fatal("fixture not interrupted")
			}
			if err := os.Link(filepath.Join(directory, target), filepath.Join(directory, prefix+"exact")); err != nil {
				t.Fatal(err)
			}
			out, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options)
			if err != nil || out.Volume.Identity == nil || out.Volume.Identity.InitialConfig != Configured {
				t.Fatalf("exact owned hardlink install window not repaired: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(directory, prefix+"exact")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("exact linked temp not removed")
			}
			info, err := os.Lstat(filepath.Join(directory, target))
			if err != nil || !privateLocalInfo(info, maximumStateBytes) {
				t.Fatal("linked final not restored to private nlink1")
			}
		})
	}
}

func TestInitialTransactionOwnedHardlinkRepairRefusesUnmatchedPairs(t *testing.T) {
	for _, change := range []string{"third-link", "wrong-prefix", "same-prefix-second-temp", "wrong-content", "mode", "symlink-pair"} {
		t.Run(change, func(t *testing.T) {
			directory, r, keys, member, options := initialTransactionFixture(t, 0)
			if _, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
				if point == "redis-installed" {
					return errors.New("stop")
				}
				return nil
			}); err == nil {
				t.Fatal("fixture not interrupted")
			}
			prefix := ".bootstrap-redis-"
			if change == "wrong-prefix" {
				prefix = ".bootstrap-sentinel-"
			}
			temp := filepath.Join(directory, prefix+"exact")
			target := filepath.Join(directory, "redis.conf")
			if err := os.Link(target, temp); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "third-link":
				if err := os.Link(target, filepath.Join(directory, "third")); err != nil {
					t.Fatal(err)
				}
			case "same-prefix-second-temp":
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, ".bootstrap-redis-second"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-content":
				if err := os.WriteFile(target, []byte("wrong credential"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(temp, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink-pair":
				if err := os.Rename(temp, filepath.Join(directory, "unmatched")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, temp); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err == nil || out.Volume.Identity != nil {
				t.Fatal("unmatched hardlink pair accepted")
			}
			if _, err := os.Lstat(temp); err != nil {
				t.Fatal("unmatched pair was removed")
			}
		})
	}
}

func TestInitialTransactionManifestTempBeforeInstallAndFlockTimeout(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 0)
	identity := VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[0], Member: member, InitialConfig: Reserved}
	redis, sentinel, err := RenderInitialMemberConfigs(r, keys, identity, options)
	if err != nil {
		t.Fatal(err)
	}
	identity.InitialConfig = Configured
	manifest, err := json.Marshal(initialConfigManifest{Identity: identity, KeyDigest: r.KeyDigest, RedisConfig: redis, SentinelConfig: sentinel})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".bootstrap-plan-retained"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err != nil {
		t.Fatalf("exact manifest temp before install could not resume: %v", err)
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".bootstrap-lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Error(err)
		}
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	if out, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err == nil || out.Volume.Identity != nil {
		t.Fatal("held local flock did not time out")
	}
	if elapsed := time.Since(start); elapsed < 5*time.Second || elapsed > 6*time.Second {
		t.Fatalf("lock timeout outside fixed bound: %v", elapsed)
	}
}

func TestInitialTransactionPhaseMutationAndConfiguredSyncFailure(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 2)
	changed := false
	if out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
		if point == "before-phase" {
			changed = true
			data, err := os.ReadFile(filepath.Join(directory, "identity.json"))
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.ReplaceAll(data, []byte(r.MarkerIDs[2]), []byte(strings.Repeat("f", 32)))
			if err := os.WriteFile(filepath.Join(directory, "identity.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}); !changed || err == nil || out.Volume.Identity != nil {
		t.Fatal("changed marker replaced by Configured identity")
	}
	directory, r, keys, member, options = initialTransactionFixture(t, 2)
	if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err != nil {
		t.Fatal(err)
	}
	r.Cluster.Phase = Initialized
	for _, point := range []string{"sync-file:identity.json", "sync-file:redis.conf", "sync-file:sentinel.conf", "sync-file:initial-config.json", "sync-directory"} {
		called := false
		out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(checkpoint string) error {
			if checkpoint == point {
				called = true
				return syscall.EIO
			}
			return nil
		})
		if !called || err == nil || out.Volume.Identity != nil || out.Snapshot != nil {
			t.Fatal("existing Configured marker bypassed durability failure")
		}
		if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err != nil {
			t.Fatal("Configured retry did not re-confirm durability")
		}
	}
}

func TestInitialTransactionTemporaryCleanupIsExactAndResumable(t *testing.T) {
	for _, stop := range []string{"manifest-installed", "redis-installed", "sentinel-installed", "before-phase"} {
		t.Run(stop, func(t *testing.T) {
			directory, r, keys, member, options := initialTransactionFixture(t, 0)
			_, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
				if point == stop {
					return errors.New("stop")
				}
				return nil
			})
			if err == nil {
				t.Fatal("interruption not exercised")
			}
			manifest, err := os.ReadFile(filepath.Join(directory, "initial-config.json"))
			if err != nil {
				t.Fatal(err)
			}
			identity := VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[0], Member: member, InitialConfig: Reserved}
			redis, sentinel, err := RenderInitialMemberConfigs(r, keys, identity, options)
			if err != nil {
				t.Fatal(err)
			}
			identity.InitialConfig = Configured
			configured, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			for prefix, data := range map[string][]byte{".bootstrap-plan-": manifest, ".bootstrap-redis-": redis, ".bootstrap-sentinel-": sentinel, ".bootstrap-identity-": configured} {
				if err := os.WriteFile(filepath.Join(directory, prefix+"precise"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err != nil {
				t.Fatalf("valid retained transaction temps did not resume: %v", err)
			}
			for _, prefix := range []string{".bootstrap-plan-", ".bootstrap-redis-", ".bootstrap-sentinel-", ".bootstrap-identity-"} {
				if _, err := os.Lstat(filepath.Join(directory, prefix+"precise")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("exact known temporary file retained")
				}
			}
		})
	}
}

func TestInitialTransactionRejectsUnsafeRetainedFiles(t *testing.T) {
	for _, change := range []string{"identity-symlink", "identity-hardlink", "identity-mode", "manifest-symlink", "manifest-hardlink", "manifest-mode", "manifest-large", "manifest-json-duplicate", "manifest-changed-secret", "config-changed", "wrong-temp", "symlink-temp", "hardlink-temp", "temporary-directory", "unknown-temp", "phase-marker-changed", "lock-symlink", "lock-mode", "lock-data", "lock-hardlink"} {
		t.Run(change, func(t *testing.T) {
			directory, r, keys, member, options := initialTransactionFixture(t, 0)
			if !strings.HasPrefix(change, "identity-") && !strings.HasPrefix(change, "lock-") {
				if _, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error {
					if point == "redis-installed" {
						return errors.New("stop")
					}
					return nil
				}); err == nil {
					t.Fatal("fixture not interrupted")
				}
			}
			path := filepath.Join(directory, "identity.json")
			if strings.HasPrefix(change, "manifest-") {
				path = filepath.Join(directory, "initial-config.json")
			}
			if strings.HasPrefix(change, "lock-") {
				path = filepath.Join(directory, ".bootstrap-lock")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch {
			case strings.HasSuffix(change, "-symlink"):
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-original", path); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(change, "-hardlink"):
				if err := os.Link(path, path+"-extra"); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(change, "-mode"):
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case change == "manifest-large":
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maximumStateBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case change == "manifest-json-duplicate":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.Replace(data, []byte(`{"identity":`), []byte(`{"keyDigest":"duplicate","identity":`), 1)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case change == "manifest-changed-secret":
				options.DataPassword = strings.Repeat("a", 40)
			case change == "config-changed":
				if err := os.WriteFile(filepath.Join(directory, "redis.conf"), []byte("port 6380\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case change == "wrong-temp":
				if err := os.WriteFile(filepath.Join(directory, ".bootstrap-redis-retained"), []byte("wrong content"), 0600); err != nil {
					t.Fatal(err)
				}
			case change == "symlink-temp":
				if err := os.Symlink(filepath.Join(directory, "redis.conf"), filepath.Join(directory, ".bootstrap-redis-retained")); err != nil {
					t.Fatal(err)
				}
			case change == "hardlink-temp":
				// A link under the wrong transaction prefix is not an owned pair.
				if err := os.Link(filepath.Join(directory, "redis.conf"), filepath.Join(directory, ".bootstrap-sentinel-retained")); err != nil {
					t.Fatal(err)
				}
			case change == "temporary-directory":
				if err := os.Mkdir(filepath.Join(directory, ".bootstrap-redis-retained"), 0700); err != nil {
					t.Fatal(err)
				}
			case change == "unknown-temp":
				if err := os.WriteFile(filepath.Join(directory, ".redis-identity-legacy"), []byte("retained"), 0600); err != nil {
					t.Fatal(err)
				}
			case change == "phase-marker-changed":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.ReplaceAll(data, []byte(r.MarkerIDs[0]), []byte(strings.Repeat("f", 32)))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case change == "lock-data":
				if err := os.WriteFile(path, []byte("not a lock"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			out, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options)
			if err == nil || out.Volume.Identity != nil || strings.Contains(err.Error(), directory) || strings.Contains(err.Error(), options.DataPassword) {
				t.Fatal("unsafe retained file accepted or sensitive content leaked")
			}
			after, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range before {
				if entry.Name() == ".bootstrap-lock" {
					continue
				}
				found := false
				for _, candidate := range after {
					found = found || entry.Name() == candidate.Name()
				}
				if !found {
					t.Fatal("unknown/unsafe retained file was removed")
				}
			}
		})
	}
}

func TestInitialTransactionConcurrentCallsAndFlockCancellation(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 1)
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var checkpoints []string
			out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, func(point string) error { checkpoints = append(checkpoints, point); return nil })
			if err != nil {
				err = fmt.Errorf("transaction at %v: %w", checkpoints, err)
			}
			if err == nil && (out.Volume.Identity == nil || out.Volume.Identity.MarkerID != r.MarkerIDs[1]) {
				err = errors.New("wrong concurrent marker")
			}
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("cooperating local transaction failed: %v", err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".bootstrap-lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Error(err)
		}
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ConfigureInitialLocalVolume(ctx, directory, r, keys, member, options); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("local lock contention did not honor bounded context")
	}
}

func TestInitialTransactionResumesEachCheckpoint(t *testing.T) {
	for _, stop := range []string{"manifest-installed", "redis-installed", "sentinel-installed", "before-phase", "phase-installed", "sync-temp:initial-config.json", "sync-temp:redis.conf", "sync-temp:sentinel.conf", "sync-temp:identity.json", "sync-file:initial-config.json", "sync-file:redis.conf", "sync-file:sentinel.conf", "sync-file:identity.json", "sync-directory"} {
		t.Run(stop, func(t *testing.T) {
			directory, r, keys, member, options := initialTransactionFixture(t, 1)
			stopped := false
			hook := func(point string) error {
				if point == stop && !stopped {
					stopped = true
					return errors.New("deterministic interrupted durability")
				}
				return nil
			}
			out, err := configureInitialLocalVolume(context.Background(), directory, r, keys, member, options, hook)
			if !stopped || err == nil || out.Volume.Identity != nil || out.Snapshot != nil || out.ConfigDigest != "" {
				t.Fatal("interrupted installed/sync state was laundered into success")
			}
			out, err = ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options)
			if err != nil || out.Volume.Identity == nil || out.Volume.Identity.MarkerID != r.MarkerIDs[1] || out.Volume.Identity.InitialConfig != Configured {
				t.Fatalf("same-marker interrupted transaction could not resume: %v", err)
			}
		})
	}
}

func TestInitialTransactionNeverResetsConfiguredRole(t *testing.T) {
	directory, r, keys, member, options := initialTransactionFixture(t, 2)
	if _, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options); err != nil {
		t.Fatal(err)
	}
	redisBytes, err := os.ReadFile(filepath.Join(directory, "redis.conf"))
	if err != nil {
		t.Fatal("cannot read retained Redis fixture:", err)
	}
	sentinelBytes, err := os.ReadFile(filepath.Join(directory, "sentinel.conf"))
	if err != nil {
		t.Fatal("cannot read retained Sentinel fixture:", err)
	}
	redisBytes = bytes.ReplaceAll(redisBytes, []byte("replicaof \""+r.Cluster.Members[0]+"\" 6379\n"), nil)
	sentinelBytes = bytes.ReplaceAll(sentinelBytes, []byte("monitor \"sandbox\" \""+r.Cluster.Members[0]+"\""), []byte("monitor \"sandbox\" \""+member.DNS+"\""))
	sentinelBytes = bytes.ReplaceAll(sentinelBytes, []byte("config-epoch \"sandbox\" 0"), []byte("config-epoch \"sandbox\" 9"))
	sentinelBytes = bytes.ReplaceAll(sentinelBytes, []byte("current-epoch 0"), []byte("current-epoch 14"))
	for name, data := range map[string][]byte{"redis.conf": redisBytes, "sentinel.conf": sentinelBytes} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r.Cluster.Phase = Initialized
	options.DataPassword = "invalid changed budget is irrelevant for retained state"
	out, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options)
	if err != nil || out.Snapshot == nil || out.Snapshot.State.Role != Primary || out.Snapshot.State.SentinelEpoch != 9 || out.Snapshot.CurrentEpoch != 14 {
		t.Fatalf("retained promoted primary was reset or rejected: %v", err)
	}
	for name, want := range map[string][]byte{"redis.conf": redisBytes, "sentinel.conf": sentinelBytes} {
		got, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("configured role or epoch overwritten")
		}
	}
}

func TestInitialTransactionRejectsUnregisteredAndOldData(t *testing.T) {
	for _, change := range []string{"missing-identity", "wrong-marker", "wrong-key", "initialized", "old-config", "old-data", "old-directory", "bad-password", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			directory, r, keys, member, options := initialTransactionFixture(t, 0)
			ctx := context.Background()
			switch change {
			case "missing-identity":
				if err := os.Remove(filepath.Join(directory, "identity.json")); err != nil {
					t.Fatal(err)
				}
			case "wrong-marker":
				r.MarkerIDs[0] = strings.Repeat("f", 32)
			case "wrong-key":
				keys[0] = keys[1]
			case "initialized":
				r.Cluster.Phase = Initialized
			case "old-config":
				if err := os.WriteFile(filepath.Join(directory, "redis.conf"), []byte("port 6379\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "old-data":
				if err := os.WriteFile(filepath.Join(directory, "dump.rdb"), []byte("old data"), 0600); err != nil {
					t.Fatal(err)
				}
			case "old-directory":
				if err := os.Mkdir(filepath.Join(directory, "old"), 0700); err != nil {
					t.Fatal(err)
				}
			case "bad-password":
				options.DataPassword = "never echo this secret"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			out, err := ConfigureInitialLocalVolume(ctx, directory, r, keys, member, options)
			if err == nil || out.Volume.Identity != nil || strings.Contains(err.Error(), directory) || strings.Contains(err.Error(), "never echo") {
				t.Fatal("unsafe initialization accepted or sensitive error leaked")
			}
		})
	}
}

func TestInitialTransactionConfiguresRegisteredVolume(t *testing.T) {
	for ordinal := range 3 {
		directory, r, keys, member, options := initialTransactionFixture(t, ordinal)
		observation, err := ConfigureInitialLocalVolume(context.Background(), directory, r, keys, member, options)
		if err != nil {
			t.Fatalf("registered volume not configured: %v", err)
		}
		if observation.Volume.Identity == nil || observation.Volume.Identity.InitialConfig != Configured || observation.Volume.Identity.MarkerID != r.MarkerIDs[ordinal] || observation.ConfigDigest == "" || observation.Snapshot == nil {
			t.Fatal("configured observation missing fixed identity and config evidence")
		}
		for _, name := range []string{"identity.json", "redis.conf", "sentinel.conf", "initial-config.json", ".bootstrap-lock"} {
			info, err := os.Lstat(filepath.Join(directory, name))
			if err != nil || info.Mode() != 0600 {
				t.Fatalf("private transaction file unavailable: %s", name)
			}
		}
		read, err := ReadLocalVolume(context.Background(), directory, r.Cluster, member, options.MasterName)
		if err != nil || read.ConfigDigest != observation.ConfigDigest {
			t.Fatal("completed files not readable as retained configuration")
		}
	}
}

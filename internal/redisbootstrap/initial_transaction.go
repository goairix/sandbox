package redisbootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

var errInitialTransaction = errors.New("initial local configuration is unconfirmed")

type initialConfigManifest struct {
	Identity       VolumeIdentity `json:"identity"`
	KeyDigest      string         `json:"keyDigest"`
	RedisConfig    []byte         `json:"redisConfig"`
	SentinelConfig []byte         `json:"sentinelConfig"`
}

// initialRetainedFile binds successful sync to the actual inode and bytes, not
// a later pathname observation that could adopt an unconfirmed replacement.
type initialRetainedFile struct {
	name    string
	data    []byte
	info    os.FileInfo
	maximum int64
}

func validateInitialFileRecords(ctx context.Context, root *os.Root, files []initialRetainedFile) error {
	for _, file := range files {
		if err := unchangedLocalFile(ctx, root, file.name, file.maximum, file.data, file.info); err != nil {
			return err
		}
	}
	return nil
}

// ConfigureInitialLocalVolume completes a retained Pending seed grant's local
// file transaction. It never reserves missing identity, chooses a current master
// or authorizes Redis launch. The caller must trust the stable PVC mount and its
// node/UID; flock only serializes cooperating local file writers, not elections.
// Context checks and byte bounds cannot interrupt blocked kernel file I/O.
func ConfigureInitialLocalVolume(ctx context.Context, directory string, r BootstrapRegistration, keys [3]ed25519.PublicKey, member Member, options InitialConfigOptions) (LocalVolumeSnapshot, error) {
	return configureInitialLocalVolume(ctx, directory, r, keys, member, options, nil)
}

// hook is an internal interruption/failure seam, never a success override. The
// public entry point always supplies nil, and no remote input selects checkpoints.
func configureInitialLocalVolume(ctx context.Context, directory string, r BootstrapRegistration, keys [3]ed25519.PublicKey, member Member, options InitialConfigOptions, hook func(string) error) (out LocalVolumeSnapshot, resultErr error) {
	defer func() {
		if resultErr != nil {
			out = LocalVolumeSnapshot{}
			if ctx.Err() != nil {
				resultErr = ctx.Err()
			} else {
				resultErr = errInitialTransaction
			}
		}
	}()
	digest, err := PublicKeySetDigest(keys)
	if r.Validate() != nil || member.Validate(r.Cluster) != nil || err != nil || digest != r.KeyDigest {
		return out, errInitialTransaction
	}
	root, err := openLocalRoot(ctx, directory, r.Cluster, member)
	if err != nil {
		return out, err
	}
	defer func() {
		if err := root.Close(); err != nil {
			resultErr = errInitialTransaction
		}
	}()
	// Do not create a lock on a missing identity. Read and bind the identity
	// only after flock: another cooperating transaction can atomically change
	// Reserved to Configured while this caller is waiting for the same lock.
	if _, err := root.Lstat("identity.json"); err != nil {
		return out, errInitialTransaction
	}
	lock, err := acquireInitialLock(ctx, root)
	if err != nil {
		return out, err
	}
	defer func() {
		unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		closeErr := lock.Close()
		if unlockErr != nil || closeErr != nil {
			resultErr = errInitialTransaction
		}
	}()
	identity, identityBytes, identityInfo, err := initialIdentity(ctx, root, r, member)
	if err != nil {
		return out, err
	}
	if identity.InitialConfig == Configured {
		return confirmConfiguredInitial(ctx, directory, root, r.Cluster, member, options.MasterName, identity, hook)
	}
	if r.Cluster.Phase != Pending {
		return out, errInitialTransaction
	}
	redisConfig, sentinelConfig, err := RenderInitialMemberConfigs(r, keys, identity, options)
	if err != nil {
		return out, err
	}
	configured := identity
	configured.InitialConfig = Configured
	configuredBytes, err := json.Marshal(configured)
	if err != nil {
		return out, errInitialTransaction
	}
	manifest := initialConfigManifest{Identity: configured, KeyDigest: digest, RedisConfig: redisConfig, SentinelConfig: sentinelConfig}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil || len(manifestBytes) > maximumStateBytes {
		return out, errInitialTransaction
	}
	names, err := localEntries(ctx, root)
	if err != nil {
		return out, err
	}
	hasManifest := names["initial-config.json"]
	if hasManifest {
		if err := repairInitialLinkedTemp(ctx, root, names, "initial-config.json", ".bootstrap-plan-", manifestBytes, hook); err != nil {
			return out, err
		}
		if err := exactInitialManifest(ctx, root, r.Cluster, manifestBytes); err != nil {
			return out, err
		}
		for _, file := range []struct {
			target, prefix string
			data           []byte
		}{{"redis.conf", ".bootstrap-redis-", redisConfig}, {"sentinel.conf", ".bootstrap-sentinel-", sentinelConfig}} {
			if err := repairInitialLinkedTemp(ctx, root, names, file.target, file.prefix, file.data, hook); err != nil {
				return out, err
			}
		}
	}
	if err := inspectInitialEntries(ctx, root, names, hasManifest, manifestBytes, redisConfig, sentinelConfig, configuredBytes); err != nil {
		return out, err
	}
	manifestInfo, err := installInitialFile(ctx, root, "initial-config.json", ".bootstrap-plan-", manifestBytes, hook)
	if err != nil {
		return out, err
	}
	if err := initialCheckpoint(ctx, hook, "manifest-installed"); err != nil {
		return out, err
	}
	redisInfo, err := installInitialFile(ctx, root, "redis.conf", ".bootstrap-redis-", redisConfig, hook)
	if err != nil {
		return out, err
	}
	if err := initialCheckpoint(ctx, hook, "redis-installed"); err != nil {
		return out, err
	}
	sentinelInfo, err := installInitialFile(ctx, root, "sentinel.conf", ".bootstrap-sentinel-", sentinelConfig, hook)
	if err != nil {
		return out, err
	}
	if err := initialCheckpoint(ctx, hook, "sentinel-installed"); err != nil {
		return out, err
	}
	if err := syncInitialDirectory(ctx, root, hook); err != nil {
		return out, err
	}
	if err := unchangedLocalFile(ctx, root, "identity.json", maximumStateBytes, identityBytes, identityInfo); err != nil {
		return out, err
	}
	if err := initialCheckpoint(ctx, hook, "before-phase"); err != nil {
		return out, err
	}
	// A hook or cooperating writer cannot change the reservation between the
	// checkpoint and replacement; a hostile same-UID process remains out of scope.
	if err := unchangedLocalFile(ctx, root, "identity.json", maximumStateBytes, identityBytes, identityInfo); err != nil {
		return out, err
	}
	names, err = localEntries(ctx, root)
	if err != nil {
		return out, err
	}
	if err := inspectInitialEntries(ctx, root, names, true, manifestBytes, redisConfig, sentinelConfig, configuredBytes); err != nil {
		return out, err
	}
	if err := exactInitialManifest(ctx, root, r.Cluster, manifestBytes); err != nil {
		return out, err
	}
	files := []initialRetainedFile{
		{"initial-config.json", manifestBytes, manifestInfo, maximumStateBytes},
		{"redis.conf", redisConfig, redisInfo, maximumPersistentConfigBytes},
		{"sentinel.conf", sentinelConfig, sentinelInfo, maximumPersistentConfigBytes},
	}
	validateSyncedFiles := func(phaseTemp string) error {
		if err := validateInitialFileRecords(ctx, root, files); err != nil {
			return err
		}
		if err := unchangedLocalFile(ctx, root, "identity.json", maximumStateBytes, identityBytes, identityInfo); err != nil {
			return err
		}
		names, err := localEntries(ctx, root)
		if err != nil || len(names) != 6 {
			return errInitialTransaction
		}
		for _, name := range []string{"identity.json", "initial-config.json", "redis.conf", "sentinel.conf", ".bootstrap-lock", phaseTemp} {
			if !names[name] {
				return errInitialTransaction
			}
		}
		return nil
	}
	configuredInfo, err := replaceInitialIdentity(ctx, root, configuredBytes, hook, validateSyncedFiles)
	if err != nil {
		return out, err
	}
	files = append(files, initialRetainedFile{"identity.json", configuredBytes, configuredInfo, maximumStateBytes})
	if err := initialCheckpoint(ctx, hook, "phase-installed"); err != nil {
		return out, err
	}
	if err := syncInitialDirectory(ctx, root, hook); err != nil {
		return out, err
	}
	if err := validateInitialFileRecords(ctx, root, files); err != nil {
		return out, err
	}
	confirmed, err := confirmConfiguredInitial(ctx, directory, root, r.Cluster, member, options.MasterName, configured, hook)
	if err != nil {
		return out, err
	}
	// The helper's fresh observation is not allowed to replace this transaction's
	// original synced records, even after a complete Configured re-confirmation.
	if err := validateInitialFileRecords(ctx, root, files); err != nil {
		return out, err
	}
	return confirmed, nil
}

// The sole nlink=2 exception is a held-lock repair of the exact final/temp pair
// left by this transaction's exclusive hardlink installer. Nlink proves there
// is no third hardlink; no general reader accepts either file in this state.
func repairInitialLinkedTemp(ctx context.Context, root *os.Root, names map[string]bool, target, prefix string, expected []byte, hook func(string) error) error {
	if !names[target] {
		return nil
	}
	info, err := root.Lstat(target)
	if err != nil {
		return errInitialTransaction
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errInitialTransaction
	}
	if stat.Nlink == 1 {
		return nil
	}
	if !initialLinkedInfo(info, expected) {
		return errInitialTransaction
	}
	temp := ""
	for name := range names {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			if temp != "" {
				return errInitialTransaction
			}
			temp = name
		}
	}
	if temp == "" {
		return errInitialTransaction
	}
	tempInfo, err := root.Lstat(temp)
	if err != nil || !os.SameFile(info, tempInfo) || !initialLinkedInfo(tempInfo, expected) {
		return errInitialTransaction
	}
	for _, name := range []string{target, temp} {
		if err := readInitialLinkedFile(ctx, root, name, info, expected); err != nil {
			return err
		}
	}
	if err := root.Remove(temp); err != nil {
		return errInitialTransaction
	}
	delete(names, temp)
	if err := syncInitialDirectory(ctx, root, hook); err != nil {
		return err
	}
	data, current, err := readPrivateLocalFile(ctx, root, target, maximumStateBytes)
	if err != nil || !os.SameFile(info, current) || !bytes.Equal(data, expected) {
		return errInitialTransaction
	}
	return syncInitialFile(ctx, root, target, data, current, hook)
}

func initialLinkedInfo(info os.FileInfo, expected []byte) bool {
	if info == nil || info.Mode() != 0600 || info.Size() != int64(len(expected)) || info.Size() <= 0 || info.Size() > maximumStateBytes {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 2
}

func readInitialLinkedFile(ctx context.Context, root *os.Root, name string, expectedInfo os.FileInfo, expected []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errInitialTransaction
	}
	info, statErr := file.Stat()
	if statErr != nil || !initialLinkedInfo(info, expected) || !os.SameFile(info, expectedInfo) || !info.ModTime().Equal(expectedInfo.ModTime()) {
		if err := file.Close(); err != nil {
			return errInitialTransaction
		}
		return errInitialTransaction
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximumStateBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	pathInfo, pathErr := root.Lstat(name)
	if readErr != nil || statErr != nil || closeErr != nil || pathErr != nil || !bytes.Equal(data, expected) || !initialLinkedInfo(after, expected) || !initialLinkedInfo(pathInfo, expected) || !os.SameFile(info, after) || !os.SameFile(info, pathInfo) || !info.ModTime().Equal(after.ModTime()) || !info.ModTime().Equal(pathInfo.ModTime()) {
		return errInitialTransaction
	}
	return ctx.Err()
}

func initialCheckpoint(ctx context.Context, hook func(string) error, point string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		if err := hook(point); err != nil {
			return errInitialTransaction
		}
	}
	return ctx.Err()
}

func initialIdentity(ctx context.Context, root *os.Root, r BootstrapRegistration, member Member) (VolumeIdentity, []byte, os.FileInfo, error) {
	data, info, err := readPrivateLocalFile(ctx, root, "identity.json", maximumStateBytes)
	if err != nil {
		return VolumeIdentity{}, nil, nil, err
	}
	identity, err := ParseVolumeIdentity(data, r.Cluster)
	if err != nil || identity.Member != member || identity.MarkerID != r.MarkerIDs[member.Ordinal] {
		return VolumeIdentity{}, nil, nil, errInitialTransaction
	}
	return identity, data, info, nil
}

func acquireInitialLock(ctx context.Context, root *os.Root) (*os.File, error) {
	deadline := time.Now().Add(5 * time.Second)
	var file *os.File
	for {
		if ctx.Err() != nil {
			return nil, errInitialTransaction
		}
		var err error
		file, err = root.OpenFile(".bootstrap-lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err == nil {
			break
		}
		// Concurrent fixed-name creation can return ENOENT on the local test
		// filesystem despite O_CREATE. Opening a lock is not an identity/config
		// install; retry only this transient open, then validate and flock it.
		if !errors.Is(err, os.ErrNotExist) || !time.Now().Before(deadline) {
			return nil, errInitialTransaction
		}
		waitInitialLock(ctx)
	}
	valid := func() bool {
		info, err := file.Stat()
		pathInfo, pathErr := root.Lstat(".bootstrap-lock")
		if err != nil || pathErr != nil || info.Mode() != 0600 || info.Size() != 0 || !os.SameFile(info, pathInfo) {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
	}
	for {
		if ctx.Err() != nil || !valid() {
			if err := file.Close(); err != nil {
				return nil, errInitialTransaction
			}
			return nil, errInitialTransaction
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if valid() {
				return file, nil
			}
			unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			closeErr := file.Close()
			if unlockErr != nil || closeErr != nil {
				return nil, errInitialTransaction
			}
			return nil, errInitialTransaction
		}
		if (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR)) || !time.Now().Before(deadline) {
			if err := file.Close(); err != nil {
				return nil, errInitialTransaction
			}
			return nil, errInitialTransaction
		}
		waitInitialLock(ctx)
	}
}

func waitInitialLock(ctx context.Context) {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func exactInitialManifest(ctx context.Context, root *os.Root, c ClusterState, expected []byte) error {
	data, _, err := readPrivateLocalFile(ctx, root, "initial-config.json", maximumStateBytes)
	if err != nil || checkJSON(data) != nil {
		return errInitialTransaction
	}
	fields, err := exactKeys(data, "identity", "keyDigest", "redisConfig", "sentinelConfig")
	if err != nil {
		return errInitialTransaction
	}
	identity, err := ParseVolumeIdentity(fields["identity"], c)
	var parsed initialConfigManifest
	if err != nil || identity.InitialConfig != Configured || json.Unmarshal(data, &parsed) != nil || parsed.Identity != identity || !lowerHex(parsed.KeyDigest, 32) || len(parsed.RedisConfig) == 0 || len(parsed.RedisConfig) > maximumPersistentConfigBytes || len(parsed.SentinelConfig) == 0 || len(parsed.SentinelConfig) > maximumPersistentConfigBytes || !bytes.Equal(data, expected) {
		return errInitialTransaction
	}
	return nil
}

func inspectInitialEntries(ctx context.Context, root *os.Root, names map[string]bool, hasManifest bool, manifest, redis, sentinel, identity []byte) error {
	allowed := map[string]bool{"identity.json": true, ".bootstrap-lock": true}
	if hasManifest {
		allowed["initial-config.json"] = true
		allowed["redis.conf"] = true
		allowed["sentinel.conf"] = true
	}
	temps := map[string][]byte{".bootstrap-plan-": manifest}
	if hasManifest {
		temps[".bootstrap-redis-"] = redis
		temps[".bootstrap-sentinel-"] = sentinel
		temps[".bootstrap-identity-"] = identity
	}
	for name := range names {
		if allowed[name] {
			continue
		}
		var expected []byte
		for prefix, contents := range temps {
			if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
				expected = contents
				break
			}
		}
		if expected == nil {
			return errInitialTransaction
		}
		data, info, err := readPrivateLocalFile(ctx, root, name, maximumStateBytes)
		if err != nil || !bytes.Equal(data, expected) {
			return errInitialTransaction
		}
		if err := unchangedLocalFile(ctx, root, name, maximumStateBytes, data, info); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil {
			return errInitialTransaction
		}
	}
	return nil
}

func initialPrivateTemp(ctx context.Context, root *os.Root, prefix, target string, data []byte, hook func(string) error) (name string, syncedInfo os.FileInfo, resultErr error) {
	session, err := NewProofSession()
	if err != nil {
		return "", nil, errInitialTransaction
	}
	name = prefix + session
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return "", nil, errInitialTransaction
	}
	var writtenInfo os.FileInfo
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errInitialTransaction
		}
		if resultErr != nil && writtenInfo != nil {
			// A failed boundary can coincide with pathname replacement. Cleanup
			// owns only this exact written inode and bytes, not an arbitrary file
			// that happens to occupy its generated name. Keep unknown state.
			actual, info, err := readPrivateLocalFile(context.WithoutCancel(ctx), root, name, int64(len(data)))
			if err == nil && os.SameFile(writtenInfo, info) && bytes.Equal(actual, data) {
				if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
					resultErr = errInitialTransaction
				}
			}
		}
	}()
	if n, err := file.Write(data); err != nil || n != len(data) {
		return name, nil, errInitialTransaction
	}
	info, err := file.Stat()
	if err != nil || !privateLocalInfo(info, int64(len(data))) {
		return name, nil, errInitialTransaction
	}
	writtenInfo = info
	if err := initialCheckpoint(ctx, hook, "sync-temp:"+target); err != nil {
		return name, nil, err
	}
	if err := file.Sync(); err != nil {
		return name, nil, errInitialTransaction
	}
	if err := unchangedLocalFile(ctx, root, name, int64(len(data)), data, info); err != nil {
		return name, nil, err
	}
	return name, info, nil
}

func installInitialFile(ctx context.Context, root *os.Root, target, prefix string, data []byte, hook func(string) error) (os.FileInfo, error) {
	info, err := root.Lstat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errInitialTransaction
	}
	if err != nil {
		temp, _, err := initialPrivateTemp(ctx, root, prefix, target, data, hook)
		if err != nil {
			return nil, err
		}
		linkErr := root.Link(temp, target)
		removeErr := root.Remove(temp)
		if removeErr != nil || (linkErr != nil && !errors.Is(linkErr, os.ErrExist)) {
			return nil, errInitialTransaction
		}
		// Only a true exclusive-install loser may inspect the winner. Sync or
		// other uncertain failures never become success by rereading.
		if err := syncInitialDirectory(ctx, root, hook); err != nil {
			return nil, err
		}
	} else if !privateLocalInfo(info, maximumPersistentConfigBytes) {
		return nil, errInitialTransaction
	}
	actual, info, err := readPrivateLocalFile(ctx, root, target, maximumPersistentConfigBytes)
	if err != nil || !bytes.Equal(actual, data) {
		return nil, errInitialTransaction
	}
	if err := syncInitialFile(ctx, root, target, actual, info, hook); err != nil {
		return nil, err
	}
	return info, nil
}

func replaceInitialIdentity(ctx context.Context, root *os.Root, data []byte, hook func(string) error, validateSyncedFiles func(string) error) (os.FileInfo, error) {
	temp, syncedInfo, err := initialPrivateTemp(ctx, root, ".bootstrap-identity-", "identity.json", data, hook)
	if err != nil {
		return nil, err
	}
	// Confirm the exact previously-synced inodes after the phase temp itself
	// is synced, immediately before rename. A fresh matching byte read cannot
	// authorize replacing a different, not-yet-confirmed configuration inode.
	if err := validateSyncedFiles(temp); err != nil {
		if removeErr := root.Remove(temp); removeErr != nil {
			return nil, errInitialTransaction
		}
		return nil, err
	}
	renameErr := root.Rename(temp, "identity.json")
	if renameErr != nil {
		if err := root.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errInitialTransaction
		}
		return nil, errInitialTransaction
	}
	return syncedInfo, nil
}

func syncInitialFile(ctx context.Context, root *os.Root, name string, data []byte, expected os.FileInfo, hook func(string) error) error {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errInitialTransaction
	}
	info, statErr := file.Stat()
	if statErr != nil || !stableLocalInfo(expected, info) {
		if err := file.Close(); err != nil {
			return errInitialTransaction
		}
		return errInitialTransaction
	}
	syncErr := initialCheckpoint(ctx, hook, "sync-file:"+name)
	if syncErr == nil {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errInitialTransaction
	}
	return unchangedLocalFile(ctx, root, name, maximumPersistentConfigBytes, data, expected)
}

func syncInitialDirectory(ctx context.Context, root *os.Root, hook func(string) error) error {
	if err := initialCheckpoint(ctx, hook, "sync-directory"); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return errInitialTransaction
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return errInitialTransaction
	}
	return ctx.Err()
}

func confirmConfiguredInitial(ctx context.Context, directory string, root *os.Root, c ClusterState, member Member, masterName string, expected VolumeIdentity, hook func(string) error) (LocalVolumeSnapshot, error) {
	before, err := ReadLocalVolume(ctx, directory, c, member, masterName)
	if err != nil || before.Volume.Identity == nil || *before.Volume.Identity != expected || before.Snapshot == nil {
		return LocalVolumeSnapshot{}, errInitialTransaction
	}
	files := make([]initialRetainedFile, 0, 4)
	if _, err := root.Lstat("initial-config.json"); err == nil {
		data, info, err := readPrivateLocalFile(ctx, root, "initial-config.json", maximumStateBytes)
		if err != nil {
			return LocalVolumeSnapshot{}, err
		}
		// Retained manifest bytes are private audit state, not current role or
		// epoch authority. Do not compare them with a newly rendered seed plan.
		files = append(files, initialRetainedFile{"initial-config.json", data, info, maximumStateBytes})
	} else if !errors.Is(err, os.ErrNotExist) {
		return LocalVolumeSnapshot{}, errInitialTransaction
	}
	for _, name := range []string{"identity.json", "redis.conf", "sentinel.conf"} {
		maximum := int64(maximumPersistentConfigBytes)
		if name == "identity.json" {
			maximum = maximumStateBytes
		}
		data, info, err := readPrivateLocalFile(ctx, root, name, maximum)
		if err != nil {
			return LocalVolumeSnapshot{}, err
		}
		files = append(files, initialRetainedFile{name, data, info, maximum})
	}
	for _, file := range files {
		if err := syncInitialFile(ctx, root, file.name, file.data, file.info, hook); err != nil {
			return LocalVolumeSnapshot{}, err
		}
	}
	if err := syncInitialDirectory(ctx, root, hook); err != nil {
		return LocalVolumeSnapshot{}, err
	}
	if err := validateInitialFileRecords(ctx, root, files); err != nil {
		return LocalVolumeSnapshot{}, err
	}
	after, err := ReadLocalVolume(ctx, directory, c, member, masterName)
	if err != nil || after.Volume.Identity == nil || *after.Volume.Identity != expected || after.Snapshot == nil || after.ConfigDigest != before.ConfigDigest || *after.Snapshot != *before.Snapshot {
		return LocalVolumeSnapshot{}, errInitialTransaction
	}
	return after, nil
}

package redisbootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// LocalVolumeSnapshot is a local retained-file observation, not election authority.
type LocalVolumeSnapshot struct {
	Volume       VolumeState
	Snapshot     *PersistentConfigSnapshot
	ConfigDigest string
}

// ReadLocalVolume reads a caller-supplied trusted local PVC mount.
func ReadLocalVolume(ctx context.Context, directory string, c ClusterState, member Member, masterName string) (out LocalVolumeSnapshot, resultErr error) {
	root, err := openLocalRoot(ctx, directory, c, member)
	if err != nil {
		return out, err
	}
	defer func() {
		if err := root.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("close local PVC root failed"))
			out = LocalVolumeSnapshot{}
		}
	}()
	names, err := localEntries(ctx, root)
	if err != nil {
		return out, err
	}
	if !names["identity.json"] {
		if len(names) != 0 {
			return out, errors.New("nonempty PVC has no retained identity")
		}
		return LocalVolumeSnapshot{Volume: VolumeState{Empty: true}}, nil
	}
	identityBytes, identityInfo, err := readPrivateLocalFile(ctx, root, "identity.json", maximumStateBytes)
	if err != nil {
		return out, err
	}
	identity, err := ParseVolumeIdentity(identityBytes, c)
	if err != nil || identity.Member != member {
		return out, errors.New("retained PVC identity is invalid for local member")
	}
	var redisBytes, sentinelBytes []byte
	var redisInfo, sentinelInfo os.FileInfo
	if identity.InitialConfig == Reserved {
		if len(names) != 1 {
			return out, errors.New("reserved PVC contains incomplete configuration or data")
		}
	} else {
		redisBytes, redisInfo, err = readPrivateLocalFile(ctx, root, "redis.conf", maximumPersistentConfigBytes)
		if err != nil {
			return out, err
		}
		sentinelBytes, sentinelInfo, err = readPrivateLocalFile(ctx, root, "sentinel.conf", maximumPersistentConfigBytes)
		if err != nil {
			return out, err
		}
	}
	if err := unchangedLocalFile(ctx, root, "identity.json", maximumStateBytes, identityBytes, identityInfo); err != nil {
		return out, err
	}
	if identity.InitialConfig == Reserved {
		after, err := localEntries(ctx, root)
		if err != nil || len(after) != 1 || !after["identity.json"] {
			return out, errors.New("reserved PVC changed during observation")
		}
		return LocalVolumeSnapshot{Volume: VolumeState{Identity: &identity}}, nil
	}
	if err := unchangedLocalFile(ctx, root, "redis.conf", maximumPersistentConfigBytes, redisBytes, redisInfo); err != nil {
		return out, err
	}
	if err := unchangedLocalFile(ctx, root, "sentinel.conf", maximumPersistentConfigBytes, sentinelBytes, sentinelInfo); err != nil {
		return out, err
	}
	snapshot, err := ParsePersistentConfigs(c, member, masterName, redisBytes, sentinelBytes)
	if err != nil {
		return out, errors.New("retained Redis and Sentinel configurations are incomplete or inconsistent")
	}
	// Include lengths to avoid ambiguous byte concatenation; no configuration
	// plaintext (which includes credentials) is returned to the remote caller.
	hash := sha256.New()
	_, err = hash.Write([]byte("sandbox/redisbootstrap/local-config/v1\x00"))
	if err != nil {
		return out, errors.New("configuration digest failed")
	}
	for _, data := range [][]byte{redisBytes, sentinelBytes} {
		length := uint64(len(data))
		var prefix [8]byte
		for i := 7; i >= 0; i-- {
			prefix[i] = byte(length)
			length >>= 8
		}
		if _, err := hash.Write(prefix[:]); err != nil {
			return out, errors.New("configuration digest failed")
		}
		if _, err := hash.Write(data); err != nil {
			return out, errors.New("configuration digest failed")
		}
	}
	return LocalVolumeSnapshot{Volume: VolumeState{Identity: &identity, Persisted: &snapshot.State}, Snapshot: &snapshot, ConfigDigest: hex.EncodeToString(hash.Sum(nil))}, nil
}

// ReserveLocalVolume exclusively creates a marker on a genuinely empty volume.
func ReserveLocalVolume(ctx context.Context, directory string, c ClusterState, member Member, masterName string) (VolumeIdentity, error) {
	return reserveLocalVolume(ctx, directory, c, member, masterName, WriteVolumeIdentity, confirmLocalReservation)
}

// The writer dependency separates exclusive filesystem installation from the
// decision/confirmation path, including installed-but-unsynced error handling.
func reserveLocalVolume(ctx context.Context, directory string, c ClusterState, member Member, masterName string, writer func(context.Context, string, VolumeIdentity, ClusterState) error, confirmer func(context.Context, string, ClusterState, Member, string, VolumeIdentity) error) (VolumeIdentity, error) {
	observation, err := ReadLocalVolume(ctx, directory, c, member, masterName)
	if err != nil {
		return VolumeIdentity{}, err
	}
	if observation.Volume.Identity != nil {
		if err := confirmer(ctx, directory, c, member, masterName, *observation.Volume.Identity); err != nil {
			return VolumeIdentity{}, err
		}
		return *observation.Volume.Identity, nil
	}
	if !observation.Volume.Empty {
		return VolumeIdentity{}, errors.New("PVC emptiness is unconfirmed")
	}
	identity, err := NewVolumeIdentity(c, member)
	if err != nil {
		return VolumeIdentity{}, errors.New("cannot generate local PVC identity")
	}
	if err := writer(ctx, filepath.Join(directory, "identity.json"), identity, c); err != nil {
		if ctx.Err() != nil {
			return VolumeIdentity{}, ctx.Err()
		}
		if !errors.Is(err, os.ErrExist) {
			return VolumeIdentity{}, errors.New("exclusive PVC identity durability is unconfirmed")
		}
		// A concurrent exclusive creator may have won. Accept only a freshly
		// checked identity; failed/partial reads stay fail-closed for a retry.
		observation, readErr := ReadLocalVolume(ctx, directory, c, member, masterName)
		if readErr == nil && observation.Volume.Identity != nil {
			if err := confirmer(ctx, directory, c, member, masterName, *observation.Volume.Identity); err != nil {
				return VolumeIdentity{}, err
			}
			return *observation.Volume.Identity, nil
		}
		return VolumeIdentity{}, errors.New("exclusive PVC identity creation is unconfirmed")
	}
	observation, err = ReadLocalVolume(ctx, directory, c, member, masterName)
	if err != nil || observation.Volume.Identity == nil || *observation.Volume.Identity != identity {
		return VolumeIdentity{}, errors.New("created PVC reservation is unconfirmed or changed")
	}
	if err := confirmer(ctx, directory, c, member, masterName, identity); err != nil {
		return VolumeIdentity{}, err
	}
	return identity, nil
}

func confirmLocalReservation(ctx context.Context, directory string, c ClusterState, member Member, masterName string, expected VolumeIdentity) error {
	if err := syncLocalIdentity(ctx, directory, c, member); err != nil {
		return err
	}
	observation, err := ReadLocalVolume(ctx, directory, c, member, masterName)
	if err != nil || observation.Volume.Identity == nil || *observation.Volume.Identity != expected {
		return errors.New("synced PVC identity is unconfirmed or changed")
	}
	return nil
}

// An exclusive-create loser independently syncs the winner's retained identity
// and containing directory; merely rereading cannot confirm a failed fsync.
func syncLocalIdentity(ctx context.Context, directory string, c ClusterState, member Member) (resultErr error) {
	root, err := openLocalRoot(ctx, directory, c, member)
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("PVC sync root close failed"))
		}
	}()
	file, err := root.OpenFile("identity.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("cannot sync retained PVC identity")
	}
	info, statErr := file.Stat()
	var syncErr error
	if statErr == nil && privateLocalInfo(info, maximumStateBytes) {
		syncErr = file.Sync()
	} else {
		syncErr = errors.New("invalid identity sync target")
	}
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("PVC identity file sync is unconfirmed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return errors.New("cannot sync PVC directory")
	}
	syncErr = dir.Sync()
	closeErr = dir.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("PVC identity directory sync is unconfirmed")
	}
	return ctx.Err()
}

// ValidateBuiltinSentinelPassword bounds supported raw-rewrite-safe passwords.
func ValidateBuiltinSentinelPassword(value string) error {
	if len(value) < 32 || len(value) > 256 {
		return errors.New("built-in Sentinel requires a 32..256 byte rewrite-safe password")
	}
	for _, b := range []byte(value) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-' {
			continue
		}
		return errors.New("built-in Sentinel password contains unsupported rewrite characters")
	}
	return nil
}

func openLocalRoot(ctx context.Context, directory string, c ClusterState, member Member) (*os.Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := member.Validate(c); err != nil {
		return nil, errors.New("invalid local PVC membership")
	}
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) == string(filepath.Separator) {
		return nil, errors.New("explicit trusted PVC mount path required")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0002 != 0 {
		return nil, errors.New("PVC root is not a trusted directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("cannot open trusted PVC root")
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) || !opened.IsDir() || opened.Mode().Perm()&0002 != 0 {
		if err := root.Close(); err != nil {
			return nil, errors.New("PVC root validation and close failed")
		}
		return nil, errors.New("PVC mount root changed during open")
	}
	return root, nil
}

// localEntries is bounded. Only an empty lost+found is filesystem metadata;
// arbitrary directories and leftover state can never count as an empty PVC.
func localEntries(ctx context.Context, root *os.Root) (map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := root.Open(".")
	if err != nil {
		return nil, errors.New("cannot inspect PVC directory")
	}
	names, readErr := dir.Readdirnames(129)
	closeErr := dir.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil || len(names) > 128 {
		return nil, errors.New("PVC directory inspection exceeded bounds or failed")
	}
	entries := make(map[string]bool, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name != "lost+found" {
			entries[name] = true
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid PVC filesystem metadata")
		}
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, errors.New("unreadable PVC filesystem metadata")
		}
		children, readErr := file.Readdirnames(1)
		current, statErr := file.Stat()
		closeErr := file.Close()
		if len(children) != 0 || !errors.Is(readErr, io.EOF) || statErr != nil || closeErr != nil || !os.SameFile(info, current) {
			return nil, errors.New("PVC filesystem metadata is not stably empty")
		}
	}
	return entries, nil
}

func readPrivateLocalFile(ctx context.Context, root *os.Root, name string, maximum int64) ([]byte, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("required private PVC file is missing or unsafe")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, errors.New("cannot open private PVC file")
	}
	info, statErr := file.Stat()
	if statErr != nil || !privateLocalInfo(info, maximum) || !os.SameFile(before, info) {
		if err := file.Close(); err != nil {
			return nil, nil, errors.New("private PVC file validation and close failed")
		}
		return nil, nil, errors.New("private PVC file ownership, mode or identity is invalid")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	pathInfo, pathErr := root.Lstat(name)
	if readErr != nil || statErr != nil || closeErr != nil || pathErr != nil || len(data) == 0 || int64(len(data)) > maximum || !stableLocalInfo(info, after) || !stableLocalInfo(info, pathInfo) {
		return nil, nil, errors.New("private PVC file read is incomplete or changed")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

func privateLocalInfo(info os.FileInfo, maximum int64) bool {
	if info == nil || info.Mode() != 0600 || info.Size() <= 0 || info.Size() > maximum {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func stableLocalInfo(before, after os.FileInfo) bool {
	return after != nil && os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) && privateLocalInfo(after, max(before.Size(), 1))
}

func unchangedLocalFile(ctx context.Context, root *os.Root, name string, maximum int64, previous []byte, info os.FileInfo) error {
	data, current, err := readPrivateLocalFile(ctx, root, name, maximum)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || !bytes.Equal(previous, data) || !stableLocalInfo(info, current) {
		return errors.New("private PVC files changed during observation")
	}
	return nil
}

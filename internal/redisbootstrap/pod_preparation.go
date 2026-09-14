package redisbootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
)

// PreparePodOptions fixes the member ordinal and long-lived nonroot identity.
type PreparePodOptions struct{ Ordinal, UID, GID int }

var errPodPreparation = errors.New("pod mounts are unconfirmed")

// PreparePod prepares only this pod's fixed mounts. It must run once as root;
// it never modifies retained Redis files, markers, host mounts or credentials.
func PreparePod(ctx context.Context, o PreparePodOptions) error {
	return preparePod(ctx, o, podPreparationPaths{data: "/data", private: "/identity-private", tools: "/redis-tools", seed: "/identity-source/seed", public: "/identity-public/public-keys.json", executable: "/app/redis-bootstrap"})
}

type podPreparationPaths struct{ data, private, tools, seed, public, executable string }

func preparePod(ctx context.Context, o PreparePodOptions, p podPreparationPaths) (resultErr error) {
	if ctx == nil {
		return errPodPreparation
	}
	defer func() {
		if resultErr != nil {
			if ctx.Err() != nil {
				resultErr = ctx.Err()
			} else {
				resultErr = errPodPreparation
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if os.Geteuid() != 0 || o.Ordinal < 0 || o.Ordinal > 2 || o.UID < 1 || o.GID < 1 || o.UID > 2147483647 || o.GID > 2147483647 {
		return errPodPreparation
	}
	// Validate credentials and fixed image tooling before changing any mount.
	public, err := readBootstrapProjection(ctx, p.public, 1024)
	if err != nil {
		return errPodPreparation
	}
	keys, err := ParseMemberPublicKeys(public)
	if err != nil {
		return errPodPreparation
	}
	seed, err := readPreparationSource(ctx, p.seed, 32, 0400, true)
	if err != nil {
		return err
	}
	defer func() {
		for i := range seed {
			seed[i] = 0
		}
	}()
	private, err := ParseMemberPrivateSeed(seed, keys, o.Ordinal)
	if err != nil {
		return errPodPreparation
	}
	for i := range private {
		private[i] = 0
	}
	executable, err := readPreparationSource(ctx, p.executable, 64<<20, 0, false)
	if err != nil {
		return err
	}
	for _, mount := range []struct {
		path    string
		uid     int
		allowed string
		pvc     bool
	}{{p.data, o.UID, "", true}, {p.private, o.UID, "seed", false}, {p.tools, 0, "redis-bootstrap", false}} {
		root, err := openPreparationRoot(ctx, mount.path, mount.uid)
		if err != nil {
			return err
		}
		inspectErr := inspectPreparationEntries(ctx, root, mount.uid, mount.allowed, mount.pvc)
		closeErr := root.Close()
		if inspectErr != nil || closeErr != nil {
			return errPodPreparation
		}
	}
	if err := prepareMountRoot(ctx, p.data, o.UID, o.GID, 0700, true); err != nil {
		return err
	}
	if err := prepareMountRoot(ctx, p.private, o.UID, o.GID, 0700, false); err != nil {
		return err
	}
	if err := prepareMountRoot(ctx, p.tools, 0, 0, 0555, false); err != nil {
		return err
	}
	for _, target := range []struct {
		path, name string
		data       []byte
		mode       os.FileMode
		uid, gid   int
	}{{p.private, "seed", seed, 0400, o.UID, o.GID}, {p.tools, "redis-bootstrap", executable, 0555, 0, 0}} {
		root, err := openPreparationRoot(ctx, target.path, target.uid)
		if err != nil {
			return err
		}
		installErr := installPreparedFile(ctx, root, target.name, target.data, target.mode, target.uid, target.gid)
		closeErr := root.Close()
		if installErr != nil || closeErr != nil {
			return errPodPreparation
		}
	}
	return ctx.Err()
}

func readPreparationSource(ctx context.Context, path string, maximum int64, exactMode os.FileMode, projection bool) (data []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !projection {
		flags |= syscall.O_NOFOLLOW
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, errPodPreparation
	}
	defer func() {
		if file.Close() != nil {
			data = nil
			resultErr = errPodPreparation
		}
	}()
	before, err := file.Stat()
	if err != nil || !preparationFileInfo(before, maximum, exactMode, 0, -1) || (!projection && before.Mode().Perm()&0022 != 0) {
		return nil, errPodPreparation
	}
	data, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(data) == 0 || int64(len(data)) > maximum {
		return nil, errPodPreparation
	}
	after, err := file.Stat()
	pathInfo, pathErr := os.Stat(path)
	if err != nil || pathErr != nil || !samePreparationFile(before, after) || !samePreparationFile(before, pathInfo) {
		return nil, errPodPreparation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func openPreparationRoot(ctx context.Context, path string, uid int) (*os.Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || !preparationDirectoryInfo(before, uid) {
		return nil, errPodPreparation
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errPodPreparation
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) || !preparationDirectoryInfo(after, uid) {
		if root.Close() != nil {
			return nil, errPodPreparation
		}
		return nil, errPodPreparation
	}
	return root, nil
}

func preparationDirectoryInfo(info os.FileInfo, uid int) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uint32(uid))
}

func inspectPreparationEntries(ctx context.Context, root *os.Root, uid int, allowed string, pvc bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pvc {
		return checkEmptyPreparationMetadata(ctx, root, uid, false, 0, 0)
	}
	dir, err := root.Open(".")
	if err != nil {
		return errPodPreparation
	}
	names, readErr := dir.Readdirnames(3)
	closeErr := dir.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil || len(names) > 2 {
		return errPodPreparation
	}
	for _, name := range names {
		if name != allowed && name != ".prepare-"+allowed {
			return errPodPreparation
		}
	}
	return nil
}

func prepareMountRoot(ctx context.Context, path string, uid, gid int, mode os.FileMode, pvc bool) (resultErr error) {
	root, err := openPreparationRoot(ctx, path, uid)
	if err != nil {
		return err
	}
	defer func() {
		if root.Close() != nil {
			resultErr = errPodPreparation
		}
	}()
	if pvc {
		if err := checkEmptyPreparationMetadata(ctx, root, uid, false, 0, 0); err != nil {
			return err
		}
	}
	if err := changePreparationDirectory(ctx, root, ".", uid, gid, mode); err != nil {
		return err
	}
	if pvc {
		if err := checkEmptyPreparationMetadata(ctx, root, uid, true, uid, gid); err != nil {
			return err
		}
	}
	return syncPreparationDirectory(ctx, root)
}

func changePreparationDirectory(ctx context.Context, root *os.Root, name string, uid, gid int, mode os.FileMode) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := root.Lstat(name)
	if err != nil || !preparationDirectoryInfo(before, uid) {
		return errPodPreparation
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errPodPreparation
	}
	defer func() {
		if file.Close() != nil {
			resultErr = errPodPreparation
		}
	}()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !preparationDirectoryInfo(opened, uid) {
		return errPodPreparation
	}
	if err := file.Chmod(mode); err != nil {
		return errPodPreparation
	}
	stat := opened.Sys().(*syscall.Stat_t)
	if stat.Uid != uint32(uid) || stat.Gid != uint32(gid) {
		if err := file.Chown(uid, gid); err != nil {
			return errPodPreparation
		}
	}
	if err := file.Sync(); err != nil {
		return errPodPreparation
	}
	after, err := file.Stat()
	pathInfo, pathErr := root.Lstat(name)
	if err != nil || pathErr != nil || !os.SameFile(before, after) || !samePreparationFile(after, pathInfo) || after.Mode().Perm() != mode || !preparationOwner(after, uid, gid) {
		return errPodPreparation
	}
	return ctx.Err()
}

func checkEmptyPreparationMetadata(ctx context.Context, root *os.Root, uid int, change bool, targetUID, targetGID int) error {
	before, err := root.Lstat("lost+found")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !preparationDirectoryInfo(before, uid) {
		return errPodPreparation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := root.OpenFile("lost+found", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errPodPreparation
	}
	names, readErr := file.Readdirnames(1)
	after, statErr := file.Stat()
	closeErr := file.Close()
	if len(names) != 0 || !errors.Is(readErr, io.EOF) || statErr != nil || closeErr != nil || !os.SameFile(before, after) {
		return errPodPreparation
	}
	if change {
		return changePreparationDirectory(ctx, root, "lost+found", targetUID, targetGID, 0700)
	}
	return nil
}

func installPreparedFile(ctx context.Context, root *os.Root, name string, data []byte, mode os.FileMode, uid, gid int) error {
	return installPreparedFileWithHook(ctx, root, name, data, mode, uid, gid, nil)
}

func installPreparedFileWithHook(ctx context.Context, root *os.Root, name string, data []byte, mode os.FileMode, uid, gid int, hook func(string) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) == 0 || (name != "seed" && name != "redis-bootstrap") {
		return errPodPreparation
	}
	temp := ".prepare-" + name
	done, err := recoverPreparedInstall(ctx, root, name, temp, data, mode, uid, gid)
	if err != nil || done {
		return err
	}
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return errPodPreparation
	}
	closed, linked := false, false
	var retained os.FileInfo
	defer func() {
		if !closed {
			if file.Close() != nil {
				resultErr = errPodPreparation
			}
		}
		if resultErr != nil && !linked && retained != nil {
			state, err := readPreparedInstallState(context.WithoutCancel(ctx), root, temp, data, mode, uid, gid)
			if err == nil && samePreparationFile(retained, state.info) && state.info.Sys().(*syscall.Stat_t).Nlink == 1 {
				if root.Remove(temp) != nil {
					resultErr = errPodPreparation
				}
			}
		}
	}()
	retained, err = file.Stat()
	if err != nil {
		return errPodPreparation
	}
	checkpoint := func(point string) error {
		if err := initialCheckpoint(ctx, hook, point); err != nil {
			return err
		}
		state, err := readPreparedInstallState(ctx, root, temp, data, mode, uid, gid)
		if err != nil || !samePreparationFile(retained, state.info) || state.info.Sys().(*syscall.Stat_t).Nlink != 1 {
			return errPodPreparation
		}
		return nil
	}
	if err := checkpoint("prewrite"); err != nil {
		return err
	}
	if n, err := file.Write(data); err != nil || n != len(data) {
		retained, err = file.Stat()
		if err != nil {
			return errPodPreparation
		}
		return errPodPreparation
	}
	retained, err = file.Stat()
	if err != nil {
		return errPodPreparation
	}
	if err := checkpoint("afterwrite"); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return errPodPreparation
	}
	retained, err = file.Stat()
	if err != nil {
		return errPodPreparation
	}
	if err := checkpoint("beforechown"); err != nil {
		return err
	}
	if err := file.Chown(uid, gid); err != nil {
		return errPodPreparation
	}
	retained, err = file.Stat()
	if err != nil {
		return errPodPreparation
	}
	if err := file.Sync(); err != nil {
		return errPodPreparation
	}
	if err := checkpoint("afterSync"); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		closed = true
		return errPodPreparation
	}
	closed = true
	if err := checkpoint("beforelink"); err != nil {
		return err
	}
	if !preparationFileInfo(retained, int64(len(data)), mode, uid, gid) {
		return errPodPreparation
	}
	if err := root.Link(temp, name); err != nil {
		return errPodPreparation
	}
	linked = true
	if err := initialCheckpoint(ctx, hook, "afterlink"); err != nil {
		return err
	}
	if err := initialCheckpoint(ctx, hook, "beforeunlink"); err != nil {
		return err
	}
	if err := validatePreparedPair(ctx, root, name, temp, data, mode, uid, gid, retained); err != nil {
		return err
	}
	if err := root.Remove(temp); err != nil {
		return errPodPreparation
	}
	if err := syncPreparationDirectory(ctx, root); err != nil {
		return err
	}
	actual, current, err := readPreparedFile(ctx, root, name, int64(len(data)), mode, uid, gid)
	if err != nil || !bytes.Equal(actual, data) || !samePreparationFile(retained, current) {
		return errPodPreparation
	}
	return ctx.Err()
}

type preparedInstallState struct {
	info os.FileInfo
	data []byte
}

// This is specific to own prepared emptyDirs, never PVC/config identities.
// Creator-private partial temp bytes must be an exact bounded source prefix;
// protected/full-owner states and hardlink pairs require the complete source.
func readPreparedInstallState(ctx context.Context, root *os.Root, name string, data []byte, mode os.FileMode, uid, gid int) (preparedInstallState, error) {
	var out preparedInstallState
	if err := ctx.Err(); err != nil {
		return out, err
	}
	before, err := root.Lstat(name)
	validInfo := func(info os.FileInfo) bool {
		if info == nil || !info.Mode().IsRegular() || info.Mode() != info.Mode().Perm() || info.Size() < 0 || info.Size() > int64(len(data)) {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Nlink != 1 && stat.Nlink != 2) {
			return false
		}
		creator := preparationOwner(info, os.Geteuid(), os.Getegid())
		full := info.Mode() == mode && info.Size() == int64(len(data)) && (creator || preparationOwner(info, uid, gid))
		return (stat.Nlink == 1 && creator && info.Mode() == 0600) || (full && (stat.Nlink == 1 || preparationOwner(info, uid, gid)))
	}
	if err != nil || !validInfo(before) {
		return out, errPodPreparation
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return out, errPodPreparation
	}
	opened, statErr := file.Stat()
	contents, readErr := io.ReadAll(io.LimitReader(file, int64(len(data))+1))
	after, afterErr := file.Stat()
	closeErr := file.Close()
	pathInfo, pathErr := root.Lstat(name)
	if statErr != nil || readErr != nil || afterErr != nil || closeErr != nil || pathErr != nil || !samePreparationFile(before, opened) || !samePreparationFile(before, after) || !samePreparationFile(before, pathInfo) || !validInfo(after) || before.Sys().(*syscall.Stat_t).Nlink != after.Sys().(*syscall.Stat_t).Nlink || after.Sys().(*syscall.Stat_t).Nlink != pathInfo.Sys().(*syscall.Stat_t).Nlink || len(contents) > len(data) || !bytes.Equal(contents, data[:len(contents)]) || (before.Mode() == mode && !bytes.Equal(contents, data)) {
		return out, errPodPreparation
	}
	return preparedInstallState{info: before, data: contents}, ctx.Err()
}

func recoverPreparedInstall(ctx context.Context, root *os.Root, name, temp string, data []byte, mode os.FileMode, uid, gid int) (bool, error) {
	_, tempErr := root.Lstat(temp)
	_, finalErr := root.Lstat(name)
	if tempErr != nil && !errors.Is(tempErr, os.ErrNotExist) || finalErr != nil && !errors.Is(finalErr, os.ErrNotExist) {
		return false, errPodPreparation
	}
	if errors.Is(tempErr, os.ErrNotExist) {
		if errors.Is(finalErr, os.ErrNotExist) {
			return false, nil
		}
		return true, finishPreparedFinal(ctx, root, name, data, mode, uid, gid)
	}
	state, err := readPreparedInstallState(ctx, root, temp, data, mode, uid, gid)
	if err != nil {
		return false, err
	}
	if finalErr == nil {
		if err := validatePreparedPair(ctx, root, name, temp, data, mode, uid, gid, state.info); err != nil {
			return false, err
		}
		if err := root.Remove(temp); err != nil {
			return false, errPodPreparation
		}
		if err := syncPreparationDirectory(ctx, root); err != nil {
			return false, err
		}
		actual, info, err := readPreparedFile(ctx, root, name, int64(len(data)), mode, uid, gid)
		if err != nil || !bytes.Equal(actual, data) || !os.SameFile(state.info, info) {
			return false, errPodPreparation
		}
		if err := confirmPreparedFile(ctx, root, name, data, mode, uid, gid); err != nil {
			return false, err
		}
		actual, after, err := readPreparedFile(ctx, root, name, int64(len(data)), mode, uid, gid)
		if err != nil || !bytes.Equal(actual, data) || !os.SameFile(state.info, after) {
			return false, errPodPreparation
		}
		return true, nil
	}
	// A true init restart can discard only this exact source-bound owned temp.
	// Rechecking its original inode/bytes immediately before unlink preserves
	// an unknown replacement, even if it occupies this protocol's fixed name.
	current, err := readPreparedInstallState(ctx, root, temp, data, mode, uid, gid)
	if err != nil || !samePreparationFile(state.info, current.info) || !bytes.Equal(state.data, current.data) || current.info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return false, errPodPreparation
	}
	if err := root.Remove(temp); err != nil {
		return false, errPodPreparation
	}
	if err := syncPreparationDirectory(ctx, root); err != nil {
		return false, err
	}
	return false, nil
}

func validatePreparedPair(ctx context.Context, root *os.Root, name, temp string, data []byte, mode os.FileMode, uid, gid int, original os.FileInfo) error {
	for _, path := range []string{temp, name} {
		state, err := readPreparedInstallState(ctx, root, path, data, mode, uid, gid)
		if err != nil || !bytes.Equal(state.data, data) || !samePreparationFile(original, state.info) || state.info.Mode() != mode || !preparationOwner(state.info, uid, gid) || state.info.Sys().(*syscall.Stat_t).Nlink != 2 {
			return errPodPreparation
		}
	}
	return ctx.Err()
}

// Legacy final files may be metadata-completed only if the original private
// creator inode already contains the entire trusted source, never partial bytes.
func finishPreparedFinal(ctx context.Context, root *os.Root, name string, data []byte, mode os.FileMode, uid, gid int) (resultErr error) {
	state, err := readPreparedInstallState(ctx, root, name, data, mode, uid, gid)
	if err != nil || !bytes.Equal(state.data, data) || state.info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return errPodPreparation
	}
	if preparationFileInfo(state.info, int64(len(data)), mode, uid, gid) {
		if err := confirmPreparedFile(ctx, root, name, data, mode, uid, gid); err != nil {
			return err
		}
		actual, after, err := readPreparedFile(ctx, root, name, int64(len(data)), mode, uid, gid)
		if err != nil || !bytes.Equal(actual, data) || !samePreparationFile(state.info, after) {
			return errPodPreparation
		}
		return nil
	}
	if !preparationOwner(state.info, os.Geteuid(), os.Getegid()) {
		return errPodPreparation
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errPodPreparation
	}
	defer func() {
		if file.Close() != nil {
			resultErr = errPodPreparation
		}
	}()
	opened, err := file.Stat()
	if err != nil || !samePreparationFile(state.info, opened) {
		return errPodPreparation
	}
	current, err := readPreparedInstallState(ctx, root, name, data, mode, uid, gid)
	if err != nil || !samePreparationFile(state.info, current.info) || !bytes.Equal(current.data, data) {
		return errPodPreparation
	}
	if err := file.Chmod(mode); err != nil {
		return errPodPreparation
	}
	if err := file.Chown(uid, gid); err != nil {
		return errPodPreparation
	}
	if err := file.Sync(); err != nil {
		return errPodPreparation
	}
	confirmed, err := file.Stat()
	if err != nil || !preparationFileInfo(confirmed, int64(len(data)), mode, uid, gid) || !os.SameFile(state.info, confirmed) {
		return errPodPreparation
	}
	if err := syncPreparationDirectory(ctx, root); err != nil {
		return err
	}
	actual, after, err := readPreparedFile(ctx, root, name, int64(len(data)), mode, uid, gid)
	if err != nil || !bytes.Equal(actual, data) || !samePreparationFile(confirmed, after) {
		return errPodPreparation
	}
	return nil
}

func preparationOwner(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(uid) && (gid < 0 || stat.Gid == uint32(gid))
}

func preparationFileInfo(info os.FileInfo, maximum int64, mode os.FileMode, uid, gid int) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode() != info.Mode().Perm() || (mode != 0 && info.Mode() != mode) || info.Size() <= 0 || info.Size() > maximum || !preparationOwner(info, uid, gid) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func samePreparationFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && preparationOwner(b, int(a.Sys().(*syscall.Stat_t).Uid), int(a.Sys().(*syscall.Stat_t).Gid))
}

func readPreparedFile(ctx context.Context, root *os.Root, name string, maximum int64, mode os.FileMode, uid, gid int) ([]byte, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	before, err := root.Lstat(name)
	if err != nil || !preparationFileInfo(before, maximum, mode, uid, gid) {
		return nil, nil, errPodPreparation
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, errPodPreparation
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	pathInfo, pathErr := root.Lstat(name)
	if readErr != nil || statErr != nil || closeErr != nil || pathErr != nil || int64(len(data)) > maximum || !samePreparationFile(before, after) || !samePreparationFile(before, pathInfo) || !preparationFileInfo(after, maximum, mode, uid, gid) {
		return nil, nil, errPodPreparation
	}
	return data, before, ctx.Err()
}

func confirmPreparedFile(ctx context.Context, root *os.Root, name string, data []byte, mode os.FileMode, uid, gid int) error {
	actual, info, err := readPreparedFile(ctx, root, name, int64(len(data)), mode, uid, gid)
	if err != nil || !bytes.Equal(actual, data) {
		return errPodPreparation
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errPodPreparation
	}
	opened, statErr := file.Stat()
	syncErr := error(nil)
	if statErr == nil && samePreparationFile(info, opened) {
		syncErr = file.Sync()
	} else {
		syncErr = errPodPreparation
	}
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errPodPreparation
	}
	if err := syncPreparationDirectory(ctx, root); err != nil {
		return err
	}
	actual, after, err := readPreparedFile(ctx, root, name, int64(len(data)), mode, uid, gid)
	if err != nil || !bytes.Equal(actual, data) || !samePreparationFile(info, after) {
		return errPodPreparation
	}
	return nil
}

func syncPreparationDirectory(ctx context.Context, root *os.Root) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := root.Open(".")
	if err != nil {
		return errPodPreparation
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errPodPreparation
	}
	return ctx.Err()
}

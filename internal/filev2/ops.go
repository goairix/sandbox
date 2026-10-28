package filev2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type fileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// Run executes one file request. The command and path are separate argv
// fields. Read/write carry only raw bytes; metadata responses are JSON.
func Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	if len(argv) < 2 || ctx.Err() != nil {
		return errors.New("invalid or cancelled file operation")
	}
	op, root, relative, absolute, err := parseRequest(argv)
	if err != nil {
		return err
	}
	if op == "write" && stdin == nil {
		return errors.New("missing write body")
	}
	var editRequest fileEditRequest
	var grepControl grepRequest
	if op == "edit" {
		editRequest, err = decodeEditRequest(stdin)
		if err != nil {
			return err
		}
	}
	if op == "grep" {
		grepControl, err = decodeGrepRequest(stdin)
		if err != nil {
			return err
		}
	}
	if op == "stat" || op == "list" {
		if err := readMetadataControl(stdin); err != nil {
			return err
		}
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open file root: %w", err)
	}
	defer unix.Close(rootFD)

	switch op {
	case "stat":
		fd, err := openBeneath(rootFD, relative, unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), absolute)
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(toFileInfo(absolute, info))
	case "list":
		fd, err := openBeneath(rootFD, relative, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		dir := os.NewFile(uintptr(fd), absolute)
		defer dir.Close()
		if len(argv) == 4 || len(argv) == 5 {
			hideReserved := len(argv) == 5
			if hideReserved && argv[4] != "hide-reserved" {
				return errors.New("invalid list filter")
			}
			return listPage(ctx, dir, absolute, argv[2], argv[3], hideReserved, stdout)
		}
		entries, err := dir.Readdir(1001)
		if err != nil && err != io.EOF {
			return err
		}
		if len(entries) > 1000 {
			return errors.New("unpaged directory listing exceeds limit")
		}
		files := make([]fileInfo, 0, len(entries))
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			files = append(files, toFileInfo(path.Join(absolute, entry.Name()), entry))
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		return json.NewEncoder(stdout).Encode(files)
	case "read":
		fd, err := openBeneath(rootFD, relative, unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), absolute)
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrTypeMismatch
		}
		if len(argv) == 4 {
			start, startErr := strconv.ParseInt(argv[2], 10, 64)
			length, lengthErr := strconv.ParseInt(argv[3], 10, 64)
			if startErr != nil || lengthErr != nil || start < 0 || length < -1 {
				return errors.New("invalid read range")
			}
			if start > info.Size() {
				return errors.New("read range starts after EOF")
			}
			if length == -1 {
				length = info.Size() - start
			}
			if length > info.Size()-start {
				return errors.New("read range exceeds EOF")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err := io.CopyN(&contextWriter{ctx: ctx, writer: stdout}, io.NewSectionReader(file, start, length), length)
			return err
		}
		return copyWithContext(ctx, stdout, file)
	case "write":
		size, err := strconv.ParseInt(argv[2], 10, 64)
		if err != nil || size < 0 {
			return errors.New("invalid write size")
		}
		createParents := len(argv) == 4
		if createParents && argv[3] != "create-parents" {
			return errors.New("invalid write option")
		}
		var fd int
		if createParents {
			fd, err = openWriteWithParents(rootFD, relative)
		} else {
			fd, err = openBeneath(rootFD, relative, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NONBLOCK, 0644)
		}
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), absolute)
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrTypeMismatch
		}
		if err := copyExactly(ctx, file, stdin, size); err != nil {
			return err
		}
		return file.Sync()
	case "read-lines":
		start, startErr := strconv.Atoi(argv[2])
		count, countErr := strconv.Atoi(argv[3])
		if startErr != nil || countErr != nil || start < 1 || count < 0 {
			return ErrTextInvalid
		}
		fd, err := openBeneath(rootFD, relative, unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), absolute)
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrTypeMismatch
		}
		content, err := readLines(ctx, file, start, count)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(struct {
			Content string `json:"content"`
		}{content})
	case "edit":
		fd, err := openBeneath(rootFD, relative, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), absolute)
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrTypeMismatch
		}
		original, err := io.ReadAll(io.LimitReader(file, maxEditFileBytes+1))
		if err != nil {
			return err
		}
		edited, replacements, err := editText(original, editRequest.OldString, editRequest.NewString, editRequest.ReplaceAll)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := file.Truncate(0); err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := copyWithContext(ctx, file, bytes.NewReader(edited)); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(struct {
			Replacements int `json:"replacements"`
		}{replacements})
	case "glob":
		hideReserved := len(argv) == 4
		if hideReserved && argv[3] != "hide-reserved" {
			return errors.New("invalid glob filter")
		}
		return runGlob(ctx, rootFD, relative, absolute, argv[2], hideReserved, stdout)
	case "grep":
		hideReserved := len(argv) == 3
		if hideReserved && argv[2] != "hide-reserved" {
			return ErrTextInvalid
		}
		return runGrepWithBinary(ctx, rootFD, relative, absolute, grepControl, hideReserved, "/usr/local/bin/rg", stdout)
	default:
		return errors.New("unknown file operation")
	}
}

func parseRequest(argv []string) (operation, root, relative, absolute string, err error) {
	operation = argv[0]
	want := 2
	if operation == "write" {
		want = 3
		if len(argv) == 4 {
			want = 4
		}
	}
	if operation == "read" && len(argv) == 4 {
		want = 4
	}
	if operation == "read-lines" {
		want = 4
	}
	if operation == "glob" {
		want = 3
		if len(argv) == 4 {
			want = 4
		}
	}
	if operation == "grep" && len(argv) == 3 {
		want = 3
	}
	if operation == "list" && (len(argv) == 4 || len(argv) == 5) {
		want = len(argv)
	}
	if len(argv) != want || (operation != "stat" && operation != "list" && operation != "read" && operation != "write" && operation != "read-lines" && operation != "edit" && operation != "glob" && operation != "grep") {
		err = errors.New("invalid file operation arguments")
		return
	}
	requested := argv[1]
	if requested == "" || strings.IndexByte(requested, 0) >= 0 {
		err = errors.New("invalid file path")
		return
	}
	for _, part := range strings.Split(requested, "/") {
		if part == ".." {
			err = errors.New("parent traversal is forbidden")
			return
		}
	}
	switch {
	case requested == "/workspace", strings.HasPrefix(requested, "/workspace/"):
		root = "/workspace"
	case requested == "/tmp", strings.HasPrefix(requested, "/tmp/"):
		root = "/tmp"
	case strings.HasPrefix(requested, "/"):
		err = errors.New("path is outside allowed roots")
		return
	default:
		root = "/workspace"
	}
	absolute = path.Join(root, strings.TrimPrefix(requested, root))
	relative = strings.TrimPrefix(absolute, root+"/")
	if absolute == root {
		relative = "."
	}
	return
}

func toFileInfo(fullPath string, info os.FileInfo) fileInfo {
	return fileInfo{
		Name: info.Name(), Path: fullPath, Size: info.Size(),
		IsDir: info.IsDir(), ModTime: info.ModTime(),
	}
}

// Metadata calls currently have no body fields. Accept an empty body or an
// empty JSON object so a malformed control stream never reaches the file.
func readMetadataControl(src io.Reader) error {
	if src == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(src, 4097))
	if err != nil {
		return err
	}
	if len(body) > 4096 {
		return errors.New("metadata control exceeds limit")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil || len(fields) != 0 {
		return errors.New("invalid metadata control")
	}
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, err := dst.Write(buf[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func copyExactly(ctx context.Context, dst io.Writer, src io.Reader, size int64) error {
	if _, err := io.CopyN(&contextWriter{ctx: ctx, writer: dst}, src, size); err != nil {
		return err
	}
	var extra [1]byte
	n, err := io.ReadFull(src, extra[:])
	if n != 0 || err != io.EOF {
		return ErrInvalidWriteSize
	}
	return ctx.Err()
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w *contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

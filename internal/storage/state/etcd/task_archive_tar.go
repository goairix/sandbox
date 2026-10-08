package etcd

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type taskArchiveContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r taskArchiveContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

// Slots use 32 digest bytes plus a kind byte. Exactly this backing allocation
// is admitted, with load <= 1/2; no archive path string is retained.
func taskArchiveIndex(ctx context.Context, index []byte, name string, kind byte, insert bool) (byte, error) {
	digest := sha256.Sum256([]byte(name))
	slots := len(index) / int(taskArchiveSlotBytes)
	if slots < 2 || slots&(slots-1) != 0 {
		return 0, errTaskArchive
	}
	start := int(binary.LittleEndian.Uint64(digest[:8]) & uint64(slots-1))
	for probe := 0; probe < slots; probe++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		i := ((start + probe) & (slots - 1)) * int(taskArchiveSlotBytes)
		if index[i+32] == 0 {
			if !insert {
				return 0, errTaskArchive
			}
			copy(index[i:i+32], digest[:])
			index[i+32] = kind
			return kind, nil
		}
		if bytes.Equal(index[i:i+32], digest[:]) {
			if insert {
				return 0, errTaskArchive
			} // Duplicate/collision refuses.
			return index[i+32], nil
		}
	}
	return 0, errTaskArchive
}

func taskArchivePath(h *tar.Header, first bool) (string, error) {
	name := h.Name
	if len(name) > 4096 || !utf8.ValidString(name) || strings.Contains(name, "\\") {
		return "", errTaskArchive
	}
	if h.Typeflag == tar.TypeDir {
		// Explicit TypeDir names from the producer need not carry a slash.
		// Remove at most one, leaving repeated separators invalid below.
		name = strings.TrimSuffix(name, "/")
	}
	parts := strings.Split(name, "/")
	if len(parts) == 0 || len(parts)-1 > 128 || parts[0] != "workspace" || first && (name != "workspace" || h.Typeflag != tar.TypeDir) || !first && name == "workspace" {
		return "", errTaskArchive
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errTaskArchive
		}
		for _, c := range part {
			if unicode.IsControl(c) {
				return "", errTaskArchive
			}
		}
	}
	return name, nil
}

func taskArchiveValidateTar(ctx context.Context, f *os.File, total, maximum uint64, index []byte) (uint64, error) {
	if ctx == nil || total < 1536 || total > 1<<40 || total%512 != 0 {
		return 0, errTaskArchive
	}
	if err := taskArchivePhysicalTar(ctx, f, total); err != nil {
		return 0, err
	}
	reader := tar.NewReader(taskArchiveContextReader{ctx, io.NewSectionReader(f, 0, int64(total))})
	var count uint64
	var buffer [32768]byte
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if count >= maximum || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) || (h.Format != tar.FormatUSTAR && h.Format != tar.FormatPAX) || h.Mode < 0 || h.Mode&^0777 != 0 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.Linkname != "" || h.Devmajor != 0 || h.Devminor != 0 || h.Size < 0 || uint64(h.Size) > total || len(h.Xattrs) != 0 || !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() {
			return 0, errTaskArchive
		}
		for k := range h.PAXRecords {
			if k != "path" && k != "mtime" && k != "size" {
				return 0, errTaskArchive
			}
		}
		if h.Typeflag == tar.TypeDir && h.Size != 0 {
			return 0, errTaskArchive
		}
		name, err := taskArchivePath(h, count == 0)
		if err != nil {
			return 0, err
		}
		if count > 0 {
			parent := name[:strings.LastIndexByte(name, '/')]
			kind, err := taskArchiveIndex(ctx, index, parent, 0, false)
			if err != nil || kind != tar.TypeDir {
				return 0, errTaskArchive
			}
		}
		if _, err := taskArchiveIndex(ctx, index, name, h.Typeflag, true); err != nil {
			return 0, err
		}
		copied, err := io.CopyBuffer(io.Discard, reader, buffer[:])
		if err != nil || copied != h.Size {
			return 0, errTaskArchive
		}
		count++
	}
	if count == 0 {
		return 0, errTaskArchive
	}
	return count, ctx.Err()
}

// A physical pass rejects special metadata before archive/tar can process a
// regular body. Only producer PAX path/mtime/size fields are admitted. Go's
// special header limit is mirrored here, never an allocation by file size.
func taskArchivePhysicalTar(ctx context.Context, f *os.File, total uint64) error {
	var header [512]byte
	var zero [512]byte
	var offset uint64
	pending := false
	var paxSize *uint64
	for offset < total {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := f.ReadAt(header[:], int64(offset)); err != nil {
			return err
		}
		offset += 512
		if bytes.Equal(header[:], zero[:]) {
			if pending || total-offset != 512 {
				return errTaskArchive
			}
			if _, err := f.ReadAt(header[:], int64(offset)); err != nil {
				return err
			}
			if !bytes.Equal(header[:], zero[:]) {
				return errTaskArchive
			}
			return nil
		}
		field := strings.Trim(string(header[124:136]), " \x00")
		size, err := strconv.ParseUint(field, 8, 64)
		if err != nil || size > 1<<40 {
			return errTaskArchive
		}
		kind := header[156]
		switch kind {
		case tar.TypeXHeader:
			if pending || size == 0 || size > 1<<20 || size > total-offset {
				return errTaskArchive
			}
			data := make([]byte, int(size))
			if _, err := f.ReadAt(data, int64(offset)); err != nil {
				return err
			}
			paxSize, err = taskArchivePAX(data)
			if err != nil {
				return err
			}
			pending = true
		case tar.TypeReg, tar.TypeDir:
			if paxSize != nil {
				size = *paxSize
			}
			pending, paxSize = false, nil
		default:
			return errTaskArchive
		}
		padded := (size + 511) &^ uint64(511)
		if padded > total-offset {
			return errTaskArchive
		}
		offset += padded
	}
	return errTaskArchive
}

func taskArchivePAX(data []byte) (*uint64, error) {
	var size *uint64
	seen := byte(0)
	for len(data) != 0 {
		i := bytes.IndexByte(data, ' ')
		if i < 1 || i > 7 {
			return nil, errTaskArchive
		}
		n, err := strconv.Atoi(string(data[:i]))
		if err != nil || n <= i+2 || n > len(data) || data[n-1] != '\n' {
			return nil, errTaskArchive
		}
		record := data[i+1 : n-1]
		eq := bytes.IndexByte(record, '=')
		if eq <= 0 {
			return nil, errTaskArchive
		}
		var bit byte
		switch string(record[:eq]) {
		case "path":
			bit = 1
			if len(record[eq+1:]) > 4096 {
				return nil, errTaskArchive
			}
		case "mtime":
			bit = 2
		case "size":
			bit = 4
			v, err := strconv.ParseUint(string(record[eq+1:]), 10, 64)
			if err != nil || v > 1<<40 {
				return nil, errTaskArchive
			}
			size = &v
		default:
			return nil, errTaskArchive
		}
		if seen&bit != 0 {
			return nil, errTaskArchive
		}
		seen |= bit
		data = data[n:]
	}
	return size, nil
}

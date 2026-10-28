package filev2

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

type fileListPage struct {
	Files      []fileInfo `json:"files"`
	NextCursor string     `json:"next_cursor"`
}

// directoryMaxHeap keeps only the lexically smallest limit+1 names. The
// directory may contain far more entries than a public page can return.
type directoryMaxHeap []os.DirEntry

func (h directoryMaxHeap) Len() int           { return len(h) }
func (h directoryMaxHeap) Less(i, j int) bool { return h[i].Name() > h[j].Name() }
func (h directoryMaxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *directoryMaxHeap) Push(value any)    { *h = append(*h, value.(os.DirEntry)) }
func (h *directoryMaxHeap) Pop() any {
	last := len(*h) - 1
	value := (*h)[last]
	*h = (*h)[:last]
	return value
}

func listPage(ctx context.Context, dir *os.File, absolute, cursor, limitText string, hideReserved bool, output io.Writer) error {
	limit, err := strconv.Atoi(limitText)
	if err != nil || limit < 1 || limit > 1000 {
		return errors.New("invalid list limit")
	}
	nameAfter := ""
	if cursor != "" {
		if path.Clean(cursor) != cursor || path.Dir(cursor) != absolute || !strings.HasPrefix(cursor, absolute+"/") {
			return errors.New("invalid list cursor")
		}
		nameAfter = path.Base(cursor)
	}

	selected := make(directoryMaxHeap, 0, limit+1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(256)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			if hideReserved && fuseprotocol.IsReservedProbeObjectName(entry.Name()) {
				continue
			}
			if entry.Name() <= nameAfter {
				continue
			}
			if selected.Len() < limit+1 {
				heap.Push(&selected, entry)
			} else if entry.Name() < selected[0].Name() {
				selected[0] = entry
				heap.Fix(&selected, 0)
			}
		}
		if readErr == io.EOF {
			break
		}
	}

	sort.Slice(selected, func(i, j int) bool { return selected[i].Name() < selected[j].Name() })
	hasMore := selected.Len() > limit
	if hasMore {
		selected = selected[:limit]
	}
	page := fileListPage{Files: make([]fileInfo, 0, len(selected))}
	for _, entry := range selected {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		page.Files = append(page.Files, toFileInfo(path.Join(absolute, entry.Name()), info))
	}
	if hasMore {
		page.NextCursor = page.Files[len(page.Files)-1].Path
	}
	return json.NewEncoder(output).Encode(page)
}

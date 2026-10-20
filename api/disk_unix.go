//go:build unix

package api

import (
	"io/fs"
	"syscall"
)

// allocated is what a file occupies on disk, which for a database that
// preallocates its WAL or leaves holes is not its length.
func allocated(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}

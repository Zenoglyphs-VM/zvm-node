//go:build !unix

package api

import "io/fs"

// allocated falls back to a file's length where the OS does not report blocks.
func allocated(info fs.FileInfo) int64 { return info.Size() }

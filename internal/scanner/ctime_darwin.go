//go:build darwin

package scanner

import (
	"io/fs"
	"syscall"
)

func createdAt(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Birthtimespec.Sec
	}
	return 0
}

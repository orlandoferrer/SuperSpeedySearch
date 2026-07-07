//go:build !darwin

package scanner

import "io/fs"

// Birth time is not portably available outside macOS; report unknown.
func createdAt(info fs.FileInfo) int64 { return 0 }

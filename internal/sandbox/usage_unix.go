//go:build !windows

package sandbox

import (
	"io/fs"
	"syscall"
)

// usage is the disk space a file takes (its blocks, as du counts them) and
// its device and inode, so a file with several hard links counts once.
func usage(info fs.FileInfo) (int64, [2]uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size(), [2]uint64{}, false
	}
	return int64(stat.Blocks) * 512, [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}, stat.Nlink > 1
}

package sandbox

import "io/fs"

// usage is a file's size; Windows has no block counts or inodes to use.
func usage(info fs.FileInfo) (int64, [2]uint64, bool) {
	return info.Size(), [2]uint64{}, false
}

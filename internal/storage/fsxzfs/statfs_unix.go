//go:build !windows

package fsxzfs

import (
	"os"
	"path/filepath"
	"syscall"
)

// statfsUsage は path の filesystem の used / avail(bytes)。NFS 越しの ZFS dataset では
// blocks-bfree が dataset の `used`、bavail が `avail` を映す。
func statfsUsage(path string) (used, avail int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := int64(st.Bsize)
	return int64(st.Blocks-st.Bfree) * bsize, int64(st.Bavail) * bsize, nil
}

// isMountPoint は path がマウントポイントか(親ディレクトリと device が違うか)。
func isMountPoint(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	a, ok1 := fi.Sys().(*syscall.Stat_t)
	p, ok2 := parent.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, nil
	}
	return a.Dev != p.Dev, nil
}

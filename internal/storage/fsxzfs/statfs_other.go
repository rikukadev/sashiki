//go:build windows

package fsxzfs

import "errors"

func statfsUsage(string) (int64, int64, error) {
	return 0, 0, errors.New("statfs: unsupported platform")
}
func isMountPoint(string) (bool, error) { return false, errors.New("statfs: unsupported platform") }

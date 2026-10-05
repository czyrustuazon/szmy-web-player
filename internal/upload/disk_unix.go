//go:build !windows

package upload

import "syscall"

// platformFreeBytes reports the bytes available to unprivileged users on the
// file system that holds path.
func platformFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

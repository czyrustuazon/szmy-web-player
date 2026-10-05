//go:build windows

package resumable

import "errors"

// platformFreeBytes is not implemented on Windows (development only); the
// free-space check is then skipped.
func platformFreeBytes(string) (uint64, error) {
	return 0, errors.New("free space is not available on Windows")
}

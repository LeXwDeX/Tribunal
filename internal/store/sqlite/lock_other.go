//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package sqlite

import "os"

const dataDirLockSupported = false

// lockFile is a no-op where flock is unavailable, including Windows. The
// supported deployment is the Linux container image.
func lockFile(*os.File) error {
	return nil
}

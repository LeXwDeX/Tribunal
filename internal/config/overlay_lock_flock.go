//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package config

import (
	"errors"
	"os"
	"syscall"
)

// lockOverlayFlock takes a blocking exclusive flock. The lock is released when
// the file is closed or the process exits.
func lockOverlayFlock(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return err
		}
	}
}

// overlayWriteUnavailable reports filesystem errors that mean the overlay
// cannot be written at all, as opposed to a transient write failure.
func overlayWriteUnavailable(err error) bool {
	return errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ENOTSUP)
}

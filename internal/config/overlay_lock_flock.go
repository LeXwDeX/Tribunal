//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package config

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// lockOverlayFlock takes an exclusive flock without blocking indefinitely. It
// polls so a canceled request can stop waiting; closing the file or exiting
// the process releases the lock.
func lockOverlayFlock(ctx context.Context, file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
		default:
			return err
		}
	}
}

// openOverlayLock opens the sidecar lock file and refuses to follow a symlink
// even if one appears between the Lstat check and this call.
func openOverlayLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}

func overlayLockSymlinkError(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

// overlayWriteUnavailable reports filesystem errors that mean the overlay
// cannot be written at all, as opposed to a transient write failure.
func overlayWriteUnavailable(err error) bool {
	return errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ENOTSUP)
}

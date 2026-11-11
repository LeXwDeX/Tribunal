//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package config

import (
	"context"
	"os"
)

// lockOverlayFlock is a no-op where flock is unavailable. The supported
// deployment is the Linux container image.
func lockOverlayFlock(context.Context, *os.File) error { return nil }

func openOverlayLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

func overlayLockSymlinkError(error) bool { return false }

func overlayWriteUnavailable(error) bool { return false }

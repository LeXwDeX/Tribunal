//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package config

import "os"

// lockOverlayFlock is a no-op where flock is unavailable. The supported
// deployment is the Linux container image.
func lockOverlayFlock(*os.File) error { return nil }

func overlayWriteUnavailable(error) bool { return false }

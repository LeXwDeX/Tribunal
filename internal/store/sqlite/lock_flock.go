//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package sqlite

import (
	"errors"
	"os"
	"syscall"
)

const dataDirLockSupported = true

// lockFile takes a non-blocking flock. flock locks belong to the open file
// description, so a second open of the same file conflicts even within one
// process, and closing the descriptor or exiting releases the lock.
func lockFile(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return ErrDataDirLocked
		default:
			return errors.New("sqlite: cannot lock data directory")
		}
	}
}

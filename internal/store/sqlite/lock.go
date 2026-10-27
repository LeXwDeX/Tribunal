package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// LockFileName is the advisory lock file created inside the data directory.
const LockFileName = "tribunal.lock"

// ErrDataDirLocked reports that another process holds the data directory lock.
var ErrDataDirLocked = errors.New("sqlite: data directory is locked by another process")

// DataDirLock is an exclusive, process-level lock on a data directory. The
// operating system releases it when the process exits, however it exits.
type DataDirLock struct {
	path string
	once sync.Once
	file *os.File
	err  error
}

// LockDataDir takes an exclusive lock on dir/tribunal.lock without waiting.
// It returns ErrDataDirLocked when another process already holds it. On
// platforms without flock the lock file is created but not locked.
func LockDataDir(dir string) (*DataDirLock, error) {
	path := filepath.Join(dir, LockFileName)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errors.New("sqlite: cannot open data directory lock file")
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &DataDirLock{path: path, file: file}, nil
}

// Path returns the lock file path.
func (l *DataDirLock) Path() string {
	return l.path
}

// Release drops the lock. The lock file stays in place; its presence alone
// does not lock the directory. Release is safe to call more than once.
func (l *DataDirLock) Release() error {
	l.once.Do(func() {
		l.err = l.file.Close()
	})
	return l.err
}

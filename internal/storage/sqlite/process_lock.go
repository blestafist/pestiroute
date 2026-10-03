package sqlite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ProcessLock excludes another gateway process for one normalized database
// path. SQLite remains responsible for coordinating CLI transactions.
type ProcessLock struct{ file *os.File }

func AcquireProcessLock(dbPath string) (*ProcessLock, error) {
	if dbPath == "" {
		return nil, errors.New("sqlite database path is empty")
	}
	path, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite lock path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
		if parentErr != nil {
			return nil, fmt.Errorf("resolve sqlite lock directory: %w", parentErr)
		}
		path = filepath.Join(parent, filepath.Base(path))
	} else {
		return nil, fmt.Errorf("resolve sqlite lock target: %w", err)
	}
	f, err := os.OpenFile(path+".gateway.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open sqlite process lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire sqlite process lock: %w", err)
	}
	return &ProcessLock{file: f}, nil
}

func (l *ProcessLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return fmt.Errorf("unlock sqlite process lock: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close sqlite process lock: %w", closeErr)
	}
	return nil
}

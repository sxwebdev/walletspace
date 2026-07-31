//go:build unix

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var ErrAlreadyLocked = errors.New("walletspace home is already in use")

type Lock struct {
	file *os.File
}

func AcquireLock(home string) (*Lock, error) {
	if err := EnsureHome(home); err != nil {
		return nil, err
	}
	path := filepath.Join(home, "walletspace.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyLocked
		}
		return nil, fmt.Errorf("acquire process lock: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return fmt.Errorf("release process lock: %w", err)
	}
	return closeErr
}

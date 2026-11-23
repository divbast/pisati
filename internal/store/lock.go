// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// The conditions of the lock on a state directory.
var (
	// ErrLocked is a state directory whose lock another process holds, or
	// another Lock in this one.
	ErrLocked = errors.New("store: the state directory is locked")
	// ErrLockNotHeld is a Lock that does not hold the lock it must: the
	// zero Lock, one released, or one on another directory.
	ErrLockNotHeld = errors.New("store: the state directory's lock is not held")
)

// lockFile is the file in the state directory that the lock is taken on.
// It is never removed: a holder locks the file, not its name, and one who
// removed it would let the next take a lock on a new file while it still
// held its own.
const lockFile = "pisati.lock"

// Lock is the exclusive lock on a state directory: an flock on
// <state>/pisati.lock. serve holds it for as long as it runs, and Restore
// needs it. The zero Lock holds nothing. A Lock and its copies are one
// lock: releasing any of them releases it.
type Lock struct {
	held *heldLock
}

type heldLock struct {
	// dir is the state directory, as filepath.Clean leaves it.
	dir string
	// mu guards file. Restore holds it for reading while it runs, so that
	// the lock cannot be released under it.
	mu sync.RWMutex
	// file holds the flock; nil once released.
	file *os.File
}

// AcquireLock takes the lock on stateDir, an absolute path to a directory
// that exists, creating pisati.lock in it, mode 0600, if there is none. It
// does not wait: while another process, or another Lock in this one,
// holds the lock, it returns ErrLocked.
func AcquireLock(ctx context.Context, stateDir string) (_ Lock, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("store: lock %s: %w", stateDir, err)
		}
	}()
	if err := absolute(stateDir); err != nil {
		return Lock{}, err
	}
	if err := ctx.Err(); err != nil {
		return Lock{}, err
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return Lock{}, err
	}
	// A file opened through the root stays open once the root is closed.
	f, err := root.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err := errors.Join(err, root.Close()); err != nil {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		return Lock{}, err
	}
	if err := flock(f); err != nil {
		return Lock{}, errors.Join(err, f.Close())
	}
	return Lock{held: &heldLock{dir: filepath.Clean(stateDir), file: f}}, nil
}

// flock takes an exclusive flock on f without waiting, or returns
// ErrLocked. An flock belongs to the open file, not the process, so a
// second open of the same file, in this process or another, is refused.
func flock(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := rc.Control(func(fd uintptr) {
		lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
	}); err != nil {
		return err
	}
	if errors.Is(lockErr, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return lockErr
}

// Release releases the lock, once any Restore using it has returned.
// Releasing the zero Lock, or one released already, does nothing.
func (l Lock) Release() error {
	if l.held == nil {
		return nil
	}
	l.held.mu.Lock()
	defer l.held.mu.Unlock()
	f := l.held.file
	if f == nil {
		return nil
	}
	l.held.file = nil
	// Closing the only descriptor of the open file releases the flock.
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: release the lock on %s: %w", l.held.dir, err)
	}
	return nil
}

// hold keeps l from being released until the function it returns is
// called. It returns ErrLockNotHeld unless l holds the lock on dir.
func (l Lock) hold(dir string) (func(), error) {
	if l.held == nil {
		return nil, ErrLockNotHeld
	}
	l.held.mu.RLock()
	if l.held.file == nil || l.held.dir != filepath.Clean(dir) {
		l.held.mu.RUnlock()
		return nil, ErrLockNotHeld
	}
	return l.held.mu.RUnlock, nil
}

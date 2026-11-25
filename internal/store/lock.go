// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/divbast/pisati/internal/explain"
)

// The conditions of the lock on a state directory.
var (
	// ErrLocked is a state directory whose lock another process holds, or
	// another Lock in this one.
	ErrLocked = errors.New("another Pisati process holds its lock")
	// ErrLockNotHeld is a Lock that does not hold the lock it must: the
	// zero Lock, one released or being released, or, for Migrate, one on
	// a state directory other than the Store's.
	ErrLockNotHeld = errors.New("the lock on the state directory is not held")
)

// lockFile is the file in the state directory that the lock is taken on.
// It is never removed: a holder locks the file, not its name, and one who
// removed it would let the next take a lock on a new file while it still
// held its own.
const lockFile = "pisati.lock"

// Lock is the exclusive lock on a state directory: an flock on
// <state>/pisati.lock. serve holds it for as long as it runs, and Migrate
// and Restore need it, so that neither runs on a database another Pisati
// is using. The zero Lock holds nothing. A Lock and its copies are one
// lock: releasing any of them releases it. Migrate and Restore run one at
// a time under it, and one that waits for another stops, failing with its
// context's error, when its context ends.
type Lock struct {
	held *heldLock
}

type heldLock struct {
	// dir is the state directory, as filepath.Clean leaves it.
	dir string
	// slot holds a token while a Migrate or Restore runs under the lock,
	// so that the lock is not released under it, and so that two under
	// copies of one Lock never run at once: two Restores would each build
	// their copy under the same name. A channel rather than a mutex, so
	// that one waiting for the slot can stop when its context ends. Once
	// Release has the slot, it keeps it.
	slot chan struct{}
	// released is closed as Release begins: no Migrate or Restore starts
	// under the lock after that.
	released chan struct{}
	// once runs the release once; a second Release waits for the first.
	once sync.Once
	// file holds the flock. Only the release touches it.
	file *os.File
}

// AcquireLock takes the lock on stateDir, an absolute path to a directory
// that exists, creating pisati.lock in it, mode 0600, if there is none. It
// does not wait: while another process, or another Lock in this one,
// holds the lock, it returns ErrLocked.
func AcquireLock(ctx context.Context, stateDir string) (_ Lock, err error) {
	defer func() {
		if err != nil {
			err = explain.Wrap(err, "Pisati cannot lock the state directory %s", stateDir)
		}
	}()
	if err := absolute(stateDir, "store.AcquireLock"); err != nil {
		return Lock{}, err
	}
	if err := ctx.Err(); err != nil {
		return Lock{}, err
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		// The message gives the reason alone, as the words around it name
		// the directory; the chain keeps the error as it came.
		return Lock{}, explain.New(err, "%s", pathless(err))
	}
	// A file opened through the root stays open once the root is closed.
	f, err := root.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		err = explain.New(err, "Pisati cannot open the file %s: %s", filepath.Join(stateDir, lockFile), pathless(err))
	}
	if err := errors.Join(err, root.Close()); err != nil {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		return Lock{}, err
	}
	if err := flock(f); err != nil {
		return Lock{}, errors.Join(err, f.Close())
	}
	return Lock{held: &heldLock{
		dir:      filepath.Clean(stateDir),
		slot:     make(chan struct{}, 1),
		released: make(chan struct{}),
		file:     f,
	}}, nil
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

// Release releases the lock. From the moment it begins, no Migrate or
// Restore starts under the lock: one waiting for it returns ErrLockNotHeld.
// Release then waits for the one running under the lock, if there is one,
// to return, a wait that only that call's context bounds, and closes
// pisati.lock, which releases the flock. A Release made while another
// runs waits for that one, then does nothing, as does releasing the zero
// Lock or one released already.
func (l Lock) Release() error {
	if l.held == nil {
		return nil
	}
	var err error
	l.held.once.Do(func() {
		err = l.held.release()
	})
	return err
}

// release closes released, takes the slot from whatever runs under the
// lock, and closes the lock file.
func (h *heldLock) release() error {
	close(h.released)
	// hold refuses once released is closed, so the slot is never given
	// back, and only a call that holds it now delays this.
	h.slot <- struct{}{}
	// Closing the only descriptor of the open file releases the flock.
	if err := h.file.Close(); err != nil {
		return explain.Wrap(err, "Pisati cannot release the lock on the state directory %s", h.dir)
	}
	return nil
}

// hold keeps l from being released, and any other Migrate or Restore from
// running under it, until the function it returns is called, and returns
// the state directory l locks. While another runs under l, hold waits for
// it, and returns ctx's error if ctx ends first. It returns ErrLockNotHeld
// unless l holds its lock, as it no longer does once Release has begun.
func (l Lock) hold(ctx context.Context) (stateDir string, unhold func(), err error) {
	h := l.held
	if h == nil {
		return "", nil, ErrLockNotHeld
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	select {
	case h.slot <- struct{}{}:
	case <-h.released:
		return "", nil, ErrLockNotHeld
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	// select chooses at random among the cases ready at once, so the slot
	// may have come free just as Release began.
	select {
	case <-h.released:
		<-h.slot
		return "", nil, ErrLockNotHeld
	default:
	}
	return h.dir, func() { <-h.slot }, nil
}

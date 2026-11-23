// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package crypto seals what Pisati keeps secret in its database, with
// envelope encryption under XChaCha20-Poly1305.
//
// A master key, which LoadMaster reads from a file the operator names, a
// systemd credential, or a file Pisati generates on the data volume,
// wraps the data keys the database keeps in data_keys. A Keyring holds the
// data keys unwrapped, and seals values with the current one and opens
// them with whichever sealed them, as each sealed value names its key.
// Rotate makes a new current key and queues a crypto.reencrypt job, which
// a Reencrypter runs to re-seal each registered Field under it. Rewrap
// wraps the data keys under a new master key.
//
// A database is of no use without the master key its data keys are
// wrapped by: OpenKeyring refuses any other with ErrMasterMismatch, so
// back the key up with the database, and restore the two together.
package crypto

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

// The conditions the package's errors report. Each error it returns for
// one wraps it, and says which file, key or value is at fault.
var (
	// ErrInvalid is a value its rule refuses: the zero Master or
	// Keyring, a nil argument, a source or path LoadMaster cannot use, or
	// a Field whose names are not identifiers.
	ErrInvalid = errors.New("invalid")
	// ErrKeyFile is a master key file LoadMaster refuses: of the wrong
	// size or content, not a regular file, open to others in a way its
	// rules forbid, or, for the key Pisati generates, a symbolic link.
	ErrKeyFile = errors.New("refused")
	// ErrMasterMismatch is a master key that does not unwrap every data
	// key the database holds: not the key they were wrapped by, as when
	// a database is restored without its master key.
	ErrMasterMismatch = errors.New("the master key does not unwrap the data keys")
	// ErrTooShort is a sealed value shorter than any Seal makes.
	ErrTooShort = errors.New("sealed value too short")
	// ErrUnknownVersion is a sealed value of a format version Open does
	// not know.
	ErrUnknownVersion = errors.New("sealed value of an unknown version")
	// ErrUnknownKey is a sealed value under a data key the Keyring does
	// not hold.
	ErrUnknownKey = errors.New("sealed value under a data key the keyring does not hold")
	// ErrNotAuthentic is a sealed value that fails authentication: not
	// one Seal made, changed since, or opened with other associated data
	// than it was sealed with.
	ErrNotAuthentic = errors.New("sealed value failed authentication")
)

// The states of a data key, as data_keys spells them.
const (
	stateCurrent = "current"
	stateRetired = "retired"
)

// dataKey is a data key, unwrapped, ready to seal and open with. The
// cipher sits behind a function, as Master's key does, so that fmt never
// reaches the key it holds.
type dataKey struct {
	aead func() cipher.AEAD
}

// newDataKey returns key as a dataKey.
func newDataKey(key [keySize]byte) (dataKey, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return dataKey{}, err
	}
	return dataKey{aead: func() cipher.AEAD { return aead }}, nil
}

// Keyring holds the data keys of one database, unwrapped: the current
// one, which Seal uses, and the retired ones, which only open what they
// sealed. Make one with OpenKeyring. Its methods are safe for concurrent
// use. The zero Keyring holds no key: Seal and Open return an error
// wrapping ErrInvalid, and Rotate and NewReencrypter refuse it.
type Keyring struct {
	store  *store.Store
	master Master
	clock  func() time.Time

	// rndMu makes reads of rnd one at a time: an io.Reader need not be
	// safe for concurrent use.
	rndMu sync.Mutex
	rnd   io.Reader

	// rotating makes Rotates of one Keyring one at a time, so that each
	// retires the key the one before it made.
	rotating sync.Mutex

	mu      sync.RWMutex
	keys    map[keyID]dataKey
	current keyID
}

// OpenKeyring returns the keyring of the database s, with every data key
// in it unwrapped by m. When s holds no data key yet, it makes the first,
// current, from 32 bytes of rnd, wrapped by m and created at clock's
// time, in one IMMEDIATE transaction.
//
// It returns an error wrapping ErrMasterMismatch, changing nothing, when s
// holds data keys and m does not unwrap every one: the database is of no
// use without the master key they were wrapped by, which must be restored
// with it. A database whose keys are not exactly one current and the rest
// retired is refused too. A nil or unopened s, the zero m, or a nil clock
// or rnd is refused with an error wrapping ErrInvalid, naming every one.
//
// The Keyring reads clock when Rotate makes a key, and rnd for every key
// and nonce, one read at a time; Pisati gives it crypto/rand.Reader.
func OpenKeyring(ctx context.Context, s *store.Store, m Master, clock func() time.Time, rnd io.Reader) (*Keyring, error) {
	var errs []error
	if s.DB() == nil {
		errs = append(errs, fmt.Errorf("crypto: open the keyring: %w store: it is not open", ErrInvalid))
	}
	if m.key == nil {
		errs = append(errs, fmt.Errorf("crypto: open the keyring: %w master key: the zero Master holds none", ErrInvalid))
	}
	if clock == nil {
		errs = append(errs, fmt.Errorf("crypto: open the keyring: %w clock: nil", ErrInvalid))
	}
	if rnd == nil {
		errs = append(errs, fmt.Errorf("crypto: open the keyring: %w random source: nil", ErrInvalid))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	k := &Keyring{store: s, master: m, clock: clock, rnd: rnd}
	var keys map[keyID]dataKey
	var current keyID
	err := s.InTx(ctx, func(q *storedb.Queries) error {
		rows, err := q.ListDataKeys(ctx)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			keys, current, err = unwrapAll(m, rows)
			return err
		}
		id, dk, err := k.addKey(ctx, q)
		if err != nil {
			return err
		}
		keys, current = map[keyID]dataKey{id: dk}, id
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("crypto: open the keyring: %w", err)
	}
	k.keys, k.current = keys, current
	return k, nil
}

// unwrapAll returns every data key of rows unwrapped by m, and which is
// current. It returns an error wrapping ErrMasterMismatch unless m unwraps
// every one, and an error unless exactly one is current and the rest
// retired.
func unwrapAll(m Master, rows []storedb.ListDataKeysRow) (map[keyID]dataKey, keyID, error) {
	keys := make(map[keyID]dataKey, len(rows))
	var current []keyID
	unwrapped := 0
	for _, row := range rows {
		id, ok := toKeyID(row.ID)
		if !ok {
			return nil, 0, fmt.Errorf("data key %d: its ID is not from 1 to %d", row.ID, uint32(math.MaxUint32))
		}
		key, err := m.unwrap(id, row.Wrapped)
		if errors.Is(err, errUnwrap) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		unwrapped++
		if keys[id], err = newDataKey(key); err != nil {
			return nil, 0, err
		}
		switch row.State {
		case stateCurrent:
			current = append(current, id)
		case stateRetired:
		default:
			return nil, 0, fmt.Errorf("data key %d: its state %q is neither %s nor %s", row.ID, row.State, stateCurrent, stateRetired)
		}
	}
	if unwrapped < len(rows) {
		return nil, 0, fmt.Errorf("%w: it unwraps %d of the %d data keys; give Pisati the master key that was backed up with this database",
			ErrMasterMismatch, unwrapped, len(rows))
	}
	if len(current) != 1 {
		return nil, 0, fmt.Errorf("the database has %d current data keys, want exactly 1", len(current))
	}
	return keys, current[0], nil
}

// addKey stores a new data key through q, which runs in a transaction,
// as the current one, wrapped by the keyring's master, and returns it.
// The caller retires the key that was current first, and installs the
// new one in the keyring only once the transaction commits.
func (k *Keyring) addKey(ctx context.Context, q *storedb.Queries) (keyID, dataKey, error) {
	var key [keySize]byte
	var nonce [nonceSize]byte
	if err := k.read(key[:], nonce[:]); err != nil {
		return 0, dataKey{}, fmt.Errorf("draw a data key: %w", err)
	}
	next, err := q.NextDataKeyID(ctx)
	if err != nil {
		return 0, dataKey{}, err
	}
	id, ok := toKeyID(next)
	if !ok {
		return 0, dataKey{}, fmt.Errorf("the next data key ID, %d, is past %d", next, uint32(math.MaxUint32))
	}
	wrapped, err := k.master.wrap(nonce, id, key)
	if err != nil {
		return 0, dataKey{}, err
	}
	if err := q.InsertCurrentDataKey(ctx, storedb.InsertCurrentDataKeyParams{
		ID:        next,
		Wrapped:   wrapped,
		CreatedAt: k.clock().UnixMilli(),
	}); err != nil {
		return 0, dataKey{}, err
	}
	dk, err := newDataKey(key)
	return id, dk, err
}

// read fills each of bufs from the random source, one caller at a time.
func (k *Keyring) read(bufs ...[]byte) error {
	k.rndMu.Lock()
	defer k.rndMu.Unlock()
	for _, b := range bufs {
		if _, err := io.ReadFull(k.rnd, b); err != nil {
			return err
		}
	}
	return nil
}

// opened returns an error wrapping ErrInvalid for a Keyring that
// OpenKeyring did not return.
func (k *Keyring) opened() error {
	if k == nil || k.keys == nil {
		return fmt.Errorf("%w keyring: it was not opened with OpenKeyring", ErrInvalid)
	}
	return nil
}

// currentKey returns the current data key and its ID.
func (k *Keyring) currentKey() (keyID, dataKey) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.current, k.keys[k.current]
}

// key returns the data key id, and whether the keyring holds it.
func (k *Keyring) key(id keyID) (dataKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	dk, ok := k.keys[id]
	return dk, ok
}

// install makes dk, the data key id, which has just been committed as the
// current one, the key Seal uses.
func (k *Keyring) install(id keyID, dk dataKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[id] = dk
	k.current = id
}

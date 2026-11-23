// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/divbast/pisati/internal/config"
)

const (
	// keySize is the size of every key, master and data: 32 bytes, as
	// XChaCha20-Poly1305 takes.
	keySize = chacha20poly1305.KeySize
	// nonceSize is XChaCha20-Poly1305's nonce, 24 bytes: long enough to
	// draw at random for every message without a care for collisions.
	nonceSize = chacha20poly1305.NonceSizeX
	// tagSize is the authentication tag each ciphertext carries.
	tagSize = chacha20poly1305.Overhead
	// wrappedSize is a wrapped data key: its nonce, then the key
	// encrypted, then the tag.
	wrappedSize = nonceSize + keySize + tagSize

	// keyChars is a key in a key file: 32 bytes in standard base64, with
	// its one character of padding.
	keyChars = 44

	// keysDir is the directory in the state directory that holds the
	// master key Pisati generates, keyName in it.
	keysDir = "keys"
	keyName = "master.key"

	// redacted is all a Master ever shows of itself.
	redacted = "[redacted]"
)

// What an error or a warning about a key file says to do.
const (
	keyFormat = "want the 32-byte key in 44 characters of standard base64, optionally followed by one newline, " +
		"as openssl rand -base64 32 writes"
	namedFix = "make it mode 0400, or 0440 for group read, owned by Pisati's user or group"
	// generatedAction is what the warning on generating a key says to do.
	generatedAction = "back up this file with the database, and restore the two together: what the database holds " +
		"sealed cannot be opened without it. To keep the key off the data volume, give its file as " +
		"keys.master_key_file or as the pisati-master-key credential"
)

// Master is the master key, which wraps the data keys the database keeps.
// It never shows the key: String, fmt (every verb), slog and
// encoding/json all show [redacted], also when a Master sits in another
// value's unexported field. Masters do not compare with ==, which would
// compare keys in variable time. The zero Master holds no key, and
// OpenKeyring, Rewrap and Rotate refuse it.
type Master struct {
	// A function rather than the key, as config.Secret holds one: fmt
	// prints a function as its address, so even where it cannot call
	// Format it prints no key. It also makes Masters incomparable.
	key func() [keySize]byte
}

// newMaster returns the Master that holds key.
func newMaster(key [keySize]byte) Master {
	return Master{key: func() [keySize]byte { return key }}
}

// String returns [redacted].
func (Master) String() string {
	return redacted
}

// Format prints [redacted], whatever the verb and flags.
func (Master) Format(f fmt.State, _ rune) {
	io.WriteString(f, redacted) //nolint:errcheck // fmt.State writes into fmt's own buffer, which cannot fail, and Format has no error to return
}

// MarshalText returns [redacted], so encoding/json and slog's handlers,
// which use it, never show the key.
func (Master) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}

// LogValue shows the key to slog as [redacted].
func (Master) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// aead returns XChaCha20-Poly1305 under m's key, or an error wrapping
// ErrInvalid for the zero Master.
func (m Master) aead() (cipher.AEAD, error) {
	if m.key == nil {
		return nil, fmt.Errorf("%w master key: the zero Master holds none", ErrInvalid)
	}
	key := m.key()
	return chacha20poly1305.NewX(key[:])
}

// keyID is a data key's ID, as the data_keys table numbers it: from 1 to
// 2^32-1, so that four bytes of a sealed value name it.
type keyID uint32

// toKeyID returns n as a keyID, and whether it is one.
func toKeyID(n int64) (keyID, bool) {
	if n < 1 || n > math.MaxUint32 {
		return 0, false
	}
	return keyID(n), true
}

// bytes returns id as a sealed value spells it: four bytes, big-endian.
func (id keyID) bytes() []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(id))
}

// wrap returns dk wrapped by m as the data key id: nonce, then dk
// encrypted with id as the associated data, so that the wrapped key opens
// only as the key of row id, never moved to another.
func (m Master) wrap(nonce [nonceSize]byte, id keyID, dk [keySize]byte) ([]byte, error) {
	aead, err := m.aead()
	if err != nil {
		return nil, err
	}
	out := make([]byte, nonceSize, wrappedSize)
	copy(out, nonce[:])
	return aead.Seal(out, nonce[:], dk[:], id.bytes()), nil
}

// errUnwrap is a wrapped key that does not open under the master key as
// the key of its row.
var errUnwrap = errors.New("the wrapped data key does not open")

// unwrap returns the data key id that m wrapped as wrapped, or errUnwrap.
func (m Master) unwrap(id keyID, wrapped []byte) ([keySize]byte, error) {
	aead, err := m.aead()
	if err != nil {
		return [keySize]byte{}, err
	}
	if len(wrapped) != wrappedSize {
		return [keySize]byte{}, errUnwrap
	}
	dk, err := aead.Open(nil, wrapped[:nonceSize], wrapped[nonceSize:], id.bytes())
	if err != nil {
		return [keySize]byte{}, errUnwrap
	}
	return [keySize]byte(dk), nil
}

// LoadMaster returns the master key from where src says it is. It checks
// ctx before each file it opens, and each error it returns names the file,
// unless the name may be the key itself.
//
// A key file holds the 32-byte key in standard base64, 44 characters,
// optionally followed by one newline: what openssl rand -base64 32
// writes. Anything else is refused with ErrKeyFile, in an error that never
// shows what the file holds.
//
// A file the operator names, config.FromFile or a config.FromCredential
// that LoadMaster opens in creds, may be reached through symbolic links,
// which it follows. The file they reach must be a regular file of the
// right size, which neither its group nor others can write, else
// ErrKeyFile. Its group may read it, as a Kubernetes secret mounted with
// fsGroup and mode 0440 needs; when others can read it too, LoadMaster
// loads the key and logs a warning naming the file and the fix. The path
// of a config.FromFile must be absolute and clean, else ErrInvalid. creds
// is nil when there is no credentials directory, and a
// config.FromCredential then gets ErrInvalid.
//
// config.Generated has the key kept on the data volume, at
// <state>/keys/master.key. When there is no such file, LoadMaster makes
// the keys directory, mode 0700, if there is none, and generates the key
// from crypto/rand into the file with O_EXCL, mode 0600, flushing both to
// stable storage, and logs one warning that the key lives on the data
// volume and must be backed up and restored with the database. Pisati
// alone writes this file, so it is held to stricter rules than a file
// the operator names: LoadMaster refuses, with ErrKeyFile and an error
// that says what to do, a keys directory or key file that is a symbolic
// link or that its group or others may use in any way, a key file that is
// not a regular file, and one of the wrong size or content. A refusal
// changes nothing on disk. state must be the state directory, opened with
// os.OpenRoot; a nil state gets ErrInvalid.
//
// log takes the warnings, which never hold the key; a nil log, or a nil
// src, gets ErrInvalid.
func LoadMaster(ctx context.Context, src config.SecretSource, creds fs.FS, state *os.Root, log *slog.Logger) (Master, error) {
	if log == nil {
		return Master{}, fmt.Errorf("crypto: load the master key: %w logger: nil", ErrInvalid)
	}
	switch s := src.(type) {
	case config.FromFile:
		return loadFile(ctx, s.Path, log)
	case config.FromCredential:
		return loadCredential(ctx, creds, s.Name, log)
	case config.Generated:
		return loadGenerated(ctx, state, log)
	default:
		// nil, or a type outside config that embeds a source: a bug,
		// which must load no key.
		return Master{}, fmt.Errorf("crypto: load the master key: %w source: %T", ErrInvalid, src)
	}
}

// loadFile loads the key from the file at path, which the operator named.
func loadFile(ctx context.Context, path string, log *slog.Logger) (Master, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || looksLikeKey(path) {
		return Master{}, fmt.Errorf("crypto: load the master key: %w key file path: want an absolute, clean path to "+
			"the file that holds the key (the value is not shown, as it may be the key itself)", ErrInvalid)
	}
	where := "master key file " + path
	if err := ctx.Err(); err != nil {
		return Master{}, fmt.Errorf("crypto: %s: %w", where, err)
	}
	// Non-blocking, so that a named pipe in the key file's place is
	// refused, as no regular file, rather than waited on for a writer.
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Master{}, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	key, othersRead, err := readNamed(f, where)
	if err := errors.Join(err, f.Close()); err != nil {
		return Master{}, err
	}
	if othersRead {
		log.WarnContext(ctx, "master key file readable by others", "path", path, "fix", namedFix)
	}
	return newMaster(key), nil
}

// loadCredential loads the key from the credential name in creds.
func loadCredential(ctx context.Context, creds fs.FS, name string, log *slog.Logger) (Master, error) {
	if creds == nil {
		return Master{}, fmt.Errorf("crypto: load the master key: %w credential %q: there is no credentials directory", ErrInvalid, name)
	}
	if !fs.ValidPath(name) || name == "." || strings.Contains(name, "/") {
		return Master{}, fmt.Errorf("crypto: load the master key: %w credential name %q: want a file's name", ErrInvalid, name)
	}
	where := "master key credential " + name
	if err := ctx.Err(); err != nil {
		return Master{}, fmt.Errorf("crypto: %s: %w", where, err)
	}
	// Checked before it is opened, as fs.FS opens nothing non-blocking:
	// a named pipe there is refused rather than waited on. The file it
	// opens is checked again, as what it reads.
	fi, err := fs.Stat(creds, name)
	if err != nil {
		return Master{}, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	if err := checkNamed(fi, where); err != nil {
		return Master{}, err
	}
	f, err := creds.Open(name)
	if err != nil {
		return Master{}, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	key, othersRead, err := readNamed(f, where)
	if err := errors.Join(err, f.Close()); err != nil {
		return Master{}, err
	}
	if othersRead {
		log.WarnContext(ctx, "master key file readable by others", "credential", name, "fix", namedFix)
	}
	return newMaster(key), nil
}

// readNamed reads the key from f, a file the operator named, once its
// own Stat passes checkNamed, and reports whether others can read it.
func readNamed(f fs.File, where string) ([keySize]byte, bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return [keySize]byte{}, false, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	if err := checkNamed(fi, where); err != nil {
		return [keySize]byte{}, false, err
	}
	key, err := readKey(f, where)
	return key, fi.Mode().Perm()&0o004 != 0, err
}

// checkNamed returns an error wrapping ErrKeyFile unless fi is a regular
// file that neither its group nor others can write, of a key file's size.
func checkNamed(fi fs.FileInfo, where string) error {
	switch mode := fi.Mode(); {
	case !mode.IsRegular():
		return fmt.Errorf("crypto: %s: %w: it is not a regular file; name the file that holds the key", where, ErrKeyFile)
	case mode.Perm()&0o022 != 0:
		return fmt.Errorf("crypto: %s: %w: its mode, %04o, lets others than its owner change it; %s",
			where, ErrKeyFile, mode.Perm(), namedFix)
	}
	return checkSize(fi.Size(), where)
}

// checkSize returns an error wrapping ErrKeyFile unless n is a key file's
// size: 44 bytes, or 45 with a newline.
func checkSize(n int64, where string) error {
	if n != keyChars && n != keyChars+1 {
		return fmt.Errorf("crypto: %s: %w: it holds %d bytes; %s", where, ErrKeyFile, n, keyFormat)
	}
	return nil
}

// readKey reads the key from r, a key file, or returns an error wrapping
// ErrKeyFile that does not show what r holds.
func readKey(r io.Reader, where string) ([keySize]byte, error) {
	// One byte more than a key file holds, so a file grown since its
	// size was checked is refused rather than cut short.
	text, err := io.ReadAll(io.LimitReader(r, keyChars+2))
	if err != nil {
		return [keySize]byte{}, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	key, ok := parseKey(text)
	if !ok {
		return [keySize]byte{}, fmt.Errorf("crypto: %s: %w: %s (what it holds is not shown)", where, ErrKeyFile, keyFormat)
	}
	return key, nil
}

// parseKey returns the key text holds, as a key file does, and whether
// it holds one. Strict decoding refuses the two bits the last character
// leaves unused unless they are zero, so that a key has one spelling.
// base64 skips newlines wherever they are, so one inside text leaves too
// few characters to decode 32 bytes.
func parseKey(text []byte) ([keySize]byte, bool) {
	text, _ = bytes.CutSuffix(text, []byte("\n"))
	if len(text) != keyChars {
		return [keySize]byte{}, false
	}
	var decoded [keySize + 1]byte
	n, err := base64.StdEncoding.Strict().Decode(decoded[:], text)
	if err != nil || n != keySize {
		return [keySize]byte{}, false
	}
	return [keySize]byte(decoded[:keySize]), true
}

// looksLikeKey reports whether path reads as a key: a key given in place
// of its file's path, which an error must not repeat.
func looksLikeKey(path string) bool {
	_, ok := parseKey([]byte(path))
	return ok
}

// pathless returns the error inside err when err is an *fs.PathError,
// whose path the caller's error already names, or err.
func pathless(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// loadGenerated loads the key Pisati keeps at <state>/keys/master.key,
// generating it first if there is none.
func loadGenerated(ctx context.Context, state *os.Root, log *slog.Logger) (Master, error) {
	if state == nil {
		return Master{}, fmt.Errorf("crypto: load the master key: %w state directory: nil, so there is nowhere to keep a generated key", ErrInvalid)
	}
	dirPath := filepath.Join(state.Name(), keysDir)
	keyPath := filepath.Join(dirPath, keyName)
	name := filepath.Join(keysDir, keyName)
	where := "master key file " + keyPath
	if err := ctx.Err(); err != nil {
		return Master{}, fmt.Errorf("crypto: %s: %w", where, err)
	}
	if err := keysDirectory(state, dirPath); err != nil {
		return Master{}, err
	}
	fi, err := state.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return generate(ctx, state, name, keyPath, log)
	case err != nil:
		return Master{}, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	if err := checkGenerated(fi, keyPath); err != nil {
		return Master{}, err
	}
	f, err := state.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Master{}, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	key, err := readGenerated(f, fi, where)
	if err := errors.Join(err, f.Close()); err != nil {
		return Master{}, err
	}
	return newMaster(key), nil
}

// keysDirectory makes the keys directory in state, mode 0700, when there
// is none, and returns an error wrapping ErrKeyFile unless it is a
// directory, not a symbolic link, that neither its group nor others can
// use.
func keysDirectory(state *os.Root, dirPath string) error {
	fi, err := state.Lstat(keysDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := state.Mkdir(keysDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("crypto: make %s: %w", dirPath, pathless(err))
		}
		fi, err = state.Lstat(keysDir)
	}
	if err != nil {
		return fmt.Errorf("crypto: %s: %w", dirPath, pathless(err))
	}
	switch mode := fi.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return fmt.Errorf("crypto: %s: %w: it is a symbolic link; Pisati makes this directory itself, mode 0700, "+
			"for the key it generates: put the directory itself in the link's place", dirPath, ErrKeyFile)
	case !mode.IsDir():
		return fmt.Errorf("crypto: %s: %w: it is not a directory; Pisati keeps the key it generates in a directory "+
			"of this name, mode 0700: move what is there away", dirPath, ErrKeyFile)
	case mode.Perm()&0o077 != 0:
		return fmt.Errorf("crypto: %s: %w: its mode, %04o, lets others than its owner in; chmod 0700 %s",
			dirPath, ErrKeyFile, mode.Perm(), dirPath)
	}
	return nil
}

// checkGenerated returns an error wrapping ErrKeyFile unless fi, from
// Lstat, is a regular file, not a symbolic link, that neither its group
// nor others can use, of a key file's size.
func checkGenerated(fi fs.FileInfo, keyPath string) error {
	where := "master key file " + keyPath
	switch mode := fi.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return fmt.Errorf("crypto: %s: %w: it is a symbolic link; Pisati generated this key and never links it: "+
			"put the key file itself in the link's place, mode 0600, or give its path as keys.master_key_file", where, ErrKeyFile)
	case !mode.IsRegular():
		return fmt.Errorf("crypto: %s: %w: it is not a regular file; restore the key file backed up with the database",
			where, ErrKeyFile)
	case mode.Perm()&0o077 != 0:
		return fmt.Errorf("crypto: %s: %w: its mode, %04o, lets others than its owner read or change it; chmod 0600 %s",
			where, ErrKeyFile, mode.Perm(), keyPath)
	}
	return checkSize(fi.Size(), where)
}

// readGenerated reads the key from f, the generated key file, which must
// still be the file checked, checked as fi.
func readGenerated(f *os.File, checked fs.FileInfo, where string) ([keySize]byte, error) {
	opened, err := f.Stat()
	if err != nil {
		return [keySize]byte{}, fmt.Errorf("crypto: %s: %w", where, pathless(err))
	}
	// os.Root follows a link inside the root, so a link put in the
	// file's place since it was checked would be opened: the file opened
	// must be the one checked.
	if !os.SameFile(checked, opened) {
		return [keySize]byte{}, fmt.Errorf("crypto: %s: %w: it changed while it was read; only Pisati should write it",
			where, ErrKeyFile)
	}
	return readKey(f, where)
}

// generate generates a key into the file name in state, keyPath, which
// does not exist, and logs that it did.
func generate(ctx context.Context, state *os.Root, name, keyPath string, log *slog.Logger) (Master, error) {
	var key [keySize]byte
	if _, err := rand.Read(key[:]); err != nil {
		return Master{}, fmt.Errorf("crypto: generate the master key: %w", err)
	}
	text := append(base64.StdEncoding.AppendEncode(nil, key[:]), '\n')
	if err := writeKey(state, name, text); err != nil {
		return Master{}, fmt.Errorf("crypto: generate the master key at %s: %w", keyPath, err)
	}
	log.WarnContext(ctx, "master key generated on the data volume", "path", keyPath, "action", generatedAction)
	return newMaster(key), nil
}

// writeKey writes text to the new file name in state, mode 0600, and
// flushes it and the directories that hold it to stable storage. On a
// failure, it removes the file: no key it held was used.
func writeKey(state *os.Root, name string, text []byte) (err error) {
	f, err := state.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return pathless(err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, state.Remove(name))
		}
	}()
	_, err = f.Write(text)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	return errors.Join(syncDir(state, keysDir), syncDir(state, "."))
}

// syncDir flushes the directory name in state, and so the entries made in
// it, to stable storage.
func syncDir(state *os.Root, name string) (err error) {
	d, err := state.Open(name)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, d.Close())
	}()
	return d.Sync()
}

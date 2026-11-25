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
	"github.com/divbast/pisati/internal/explain"
)

const (
	// keySize is the size of every key, main and data: 32 bytes, as
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
	// main key Pisati generates, keyName in it.
	keysDir = "keys"
	keyName = "main.key"

	// redacted is all a MainKey ever shows of itself.
	redacted = "[redacted]"
)

// What an error or a warning about a key file says to do.
const (
	keyFormat = "A main key file must hold the 32-byte key in base64, as 44 characters, with one line break " +
		"after them or none, as the command 'openssl rand -base64 32' writes it"
	namedFix = "Give it mode 0400, or 0440 to let its group read it, and make the user or the group that runs " +
		"Pisati its owner"
	// generatedAction is what the warning on generating a key says to do.
	generatedAction = "Back up this file with the database, and restore the two together: Pisati cannot decrypt " +
		"what the database holds encrypted without this key. To keep the key off the data volume, set " +
		"keys.main_key_file (PISATI_KEYS_MAIN_KEY_FILE) to a file that holds it, or put it in the credential " +
		"pisati-main-key"
	// bug ends the error for what only a mistake in Pisati's code does.
	bug = "which is a bug in Pisati"
)

// MainKey is the main key, which wraps the data keys the database keeps.
// It never shows the key: String, fmt (every verb), slog and
// encoding/json all show [redacted], also when a MainKey sits in another
// value's unexported field. MainKeys do not compare with ==, which would
// compare keys in variable time. The zero MainKey holds no key, and
// OpenKeyring, Rewrap and Rotate refuse it.
type MainKey struct {
	// A function rather than the key, as config.Secret holds one: fmt
	// prints a function as its address, so even where it cannot call
	// Format it prints no key. It also makes MainKeys incomparable.
	key func() [keySize]byte
}

// newMainKey returns the MainKey that holds key.
func newMainKey(key [keySize]byte) MainKey {
	return MainKey{key: func() [keySize]byte { return key }}
}

// String returns [redacted].
func (MainKey) String() string {
	return redacted
}

// Format prints [redacted], whatever the verb and flags.
func (MainKey) Format(f fmt.State, _ rune) {
	io.WriteString(f, redacted) //nolint:errcheck // fmt.State writes into fmt's own buffer, which cannot fail, and Format has no error to return
}

// MarshalText returns [redacted], so encoding/json and slog's handlers,
// which use it, never show the key.
func (MainKey) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}

// LogValue shows the key to slog as [redacted].
func (MainKey) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// aead returns XChaCha20-Poly1305 under m's key, or an error wrapping
// ErrInvalid for the zero MainKey.
func (m MainKey) aead() (cipher.AEAD, error) {
	if m.key == nil {
		return nil, explain.New(ErrInvalid, "a crypto.MainKey that holds no key was used, %s", bug)
	}
	key := m.key()
	defer clear(key[:])
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
func (m MainKey) wrap(nonce [nonceSize]byte, id keyID, dk [keySize]byte) ([]byte, error) {
	aead, err := m.aead()
	if err != nil {
		return nil, err
	}
	out := make([]byte, nonceSize, wrappedSize)
	copy(out, nonce[:])
	return aead.Seal(out, nonce[:], dk[:], id.bytes()), nil
}

// errUnwrap is a wrapped key that does not open under the main key as
// the key of its row.
var errUnwrap = errors.New("the wrapped data key does not open")

// unwrap returns the data key id that m wrapped as wrapped, or errUnwrap.
func (m MainKey) unwrap(id keyID, wrapped []byte) ([keySize]byte, error) {
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
	defer clear(dk)
	return [keySize]byte(dk), nil
}

// LoadMainKey returns the main key from where src says it is. It checks
// ctx as it starts, before it touches any file, and again before it opens
// a key file to read or to write it, and returns ctx's error then. Each
// error it returns names the file, unless the name may be the key itself.
//
// A key file holds the 32-byte key in standard base64, 44 characters,
// optionally followed by one newline: what openssl rand -base64 32
// writes. Anything else is refused with ErrKeyFile, in an error that never
// shows what the file holds.
//
// A file the operator names, config.FromFile or a config.FromCredential
// that LoadMainKey opens in creds, may be reached through symbolic links,
// which it follows. The file they reach must be a regular file of the
// right size, which neither its group nor others can write, else
// ErrKeyFile. Its group may read it, as a Kubernetes secret mounted with
// fsGroup and mode 0440 needs; when others can read it too, LoadMainKey
// loads the key and logs a warning naming the file and the fix. The path
// of a config.FromFile must be absolute and clean, else ErrInvalid. creds
// is nil when there is no credentials directory, and a
// config.FromCredential then gets ErrInvalid.
//
// config.Generated has the key kept on the data volume, at
// <state>/keys/main.key. When there is no such file, LoadMainKey makes
// the keys directory, mode 0700, if there is none, and generates the key
// from crypto/rand into the file with O_EXCL, mode 0600, flushing both to
// stable storage, and logs one warning that the key lives on the data
// volume and must be backed up and restored with the database. Pisati
// alone writes this file, so it is held to stricter rules than a file
// the operator names: LoadMainKey refuses, with ErrKeyFile and an error
// that says what to do, a keys directory or key file that is a symbolic
// link or that others may use in any way, a key file that is not a
// regular file, and one of the wrong size or content. Their group may read
// and write them, and the directory may be setgid, as a Kubernetes volume
// mounted with fsGroup makes them each time it is mounted. A refusal
// changes nothing on disk. state must be the state directory, opened with
// os.OpenRoot; a nil state gets ErrInvalid.
//
// log takes the warnings, which never hold the key; a nil log, or a nil
// src, gets ErrInvalid.
func LoadMainKey(ctx context.Context, src config.SecretSource, creds fs.FS, state *os.Root, log *slog.Logger) (MainKey, error) {
	if log == nil {
		return MainKey{}, explain.New(ErrInvalid, "crypto.LoadMainKey was given no logger, %s", bug)
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
		return MainKey{}, explain.New(ErrInvalid, "crypto.LoadMainKey was given a source of the main key of the type %T, "+
			"which it does not know, %s", src, bug)
	}
}

// loadFile loads the key from the file at path, which the operator named.
func loadFile(ctx context.Context, path string, log *slog.Logger) (MainKey, error) {
	// The path is never shown: a key given by mistake in its place would be.
	const notShown = "Pisati does not show the path, because it may be the key itself"
	switch {
	case looksLikeKey(path):
		return MainKey{}, explain.New(ErrInvalid, "Pisati cannot load the main key, because the path of its file "+
			"reads as a key, not as a path. Give the full path of the file that holds the key. %s", notShown)
	case !filepath.IsAbs(path):
		return MainKey{}, explain.New(ErrInvalid, "Pisati cannot load the main key, because the path of its file is "+
			"not a full path. Give the full path of the file that holds the key, which starts with \"/\". %s", notShown)
	case filepath.Clean(path) != path:
		return MainKey{}, explain.New(ErrInvalid, "Pisati cannot load the main key, because the path of its file has "+
			"\"//\", \".\" or \"..\" in it, or ends with \"/\". Give the full path of the file, written without them. %s",
			notShown)
	}
	where := "the main key file " + path
	if err := ctx.Err(); err != nil {
		return MainKey{}, explain.Wrap(err, "Pisati stopped before it read %s", where)
	}
	// Non-blocking, so that a named pipe in the key file's place is
	// refused, as no regular file, rather than waited on for a writer.
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return MainKey{}, cannotRead(err, where, aFile)
	}
	key, othersRead, err := readNamed(f, where, aFile)
	defer clear(key[:])
	if err := errors.Join(err, f.Close()); err != nil {
		return MainKey{}, err
	}
	if othersRead {
		log.WarnContext(ctx, "main key file readable by others", "path", path, "fix", namedFix)
	}
	return newMainKey(key), nil
}

// loadCredential loads the key from the credential name in creds.
func loadCredential(ctx context.Context, creds fs.FS, name string, log *slog.Logger) (MainKey, error) {
	if creds == nil {
		return MainKey{}, explain.New(ErrInvalid, "crypto.LoadMainKey was asked for the credential %s, but given no "+
			"credentials directory, %s", name, bug)
	}
	if !fs.ValidPath(name) || name == "." || strings.Contains(name, "/") {
		return MainKey{}, explain.New(ErrInvalid, "crypto.LoadMainKey was asked for the credential %q, which is not the "+
			"name of a file, %s", name, bug)
	}
	where := "the credential " + name
	if err := ctx.Err(); err != nil {
		return MainKey{}, explain.Wrap(err, "Pisati stopped before it read %s", where)
	}
	// Checked before it is opened, as fs.FS opens nothing non-blocking:
	// a named pipe there is refused rather than waited on. The file it
	// opens is checked again, as what it reads.
	fi, err := fs.Stat(creds, name)
	if err != nil {
		return MainKey{}, cannotRead(err, where, aCredential)
	}
	if err := checkNamed(fi, where); err != nil {
		return MainKey{}, err
	}
	if err := ctx.Err(); err != nil {
		return MainKey{}, explain.Wrap(err, "Pisati stopped before it read %s", where)
	}
	f, err := creds.Open(name)
	if err != nil {
		return MainKey{}, cannotRead(err, where, aCredential)
	}
	key, othersRead, err := readNamed(f, where, aCredential)
	defer clear(key[:])
	if err := errors.Join(err, f.Close()); err != nil {
		return MainKey{}, err
	}
	if othersRead {
		log.WarnContext(ctx, "main key file readable by others", "credential", name, "fix", namedFix)
	}
	return newMainKey(key), nil
}

// readNamed reads the key from f, a file the operator named, of the
// kind named is, once its own Stat passes checkNamed, and reports whether
// others can read it.
func readNamed(f fs.File, where string, named namedKind) ([keySize]byte, bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return [keySize]byte{}, false, cannotRead(err, where, named)
	}
	if err := checkNamed(fi, where); err != nil {
		return [keySize]byte{}, false, err
	}
	key, err := readKey(f, where, named)
	return key, fi.Mode().Perm()&0o004 != 0, err
}

// checkNamed returns an error wrapping ErrKeyFile unless fi is a regular
// file that neither its group nor others can write, of a key file's size.
func checkNamed(fi fs.FileInfo, where string) error {
	switch mode := fi.Mode(); {
	case !mode.IsRegular():
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because it is not a regular file. Give the path of the "+
			"regular file that holds the key", where)
	case mode.Perm()&0o022 != 0:
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because its mode, %04o, lets users other than its owner "+
			"change it. %s", where, mode.Perm(), namedFix)
	}
	return checkSize(fi.Size(), where)
}

// checkSize returns an error wrapping ErrKeyFile unless n is a key file's
// size: 44 bytes, or 45 with a newline.
func checkSize(n int64, where string) error {
	if n != keyChars && n != keyChars+1 {
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because it holds %s, not 44 or 45 bytes. %s", where,
			explain.Bytes(n), keyFormat)
	}
	return nil
}

// readKey reads the key from r, a key file of the kind named is, or
// returns an error wrapping ErrKeyFile that does not show what r holds.
func readKey(r io.Reader, where string, named namedKind) ([keySize]byte, error) {
	// One byte more than a key file holds, so a file grown since its
	// size was checked is refused rather than cut short.
	text, err := io.ReadAll(io.LimitReader(r, keyChars+2))
	// Cleared as a best effort: the key's copies in memory are fewer,
	// though Go may have made others.
	defer clear(text)
	if err != nil {
		return [keySize]byte{}, cannotRead(err, where, named)
	}
	key, ok := parseKey(text)
	if !ok {
		return [keySize]byte{}, explain.New(ErrKeyFile, "Pisati cannot use %s, because it does not hold a key. %s. "+
			"Pisati does not show what the file holds", where, keyFormat)
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
	defer clear(decoded[:])
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

// namedKind is the kind of key file the operator names: a file, by its
// path, or a credential, in $CREDENTIALS_DIRECTORY.
type namedKind uint8

const (
	aFile namedKind = iota
	aCredential
)

// cannotRead is the error for where, a key file of the kind named is,
// that err says Pisati cannot open or read: what Pisati can tell of the
// cause from the operating system's reason, and what to check. Its cause
// is the error inside err's *fs.PathError, whose path where names.
//
// A lack of permission may come from the file's own mode or from any
// directory above it, which the operating system does not tell apart, and
// a credential's mode is set by what gives the credentials: so this error
// says what the user that runs Pisati needs, and gives no mode. Where the
// mode itself is at fault, checkNamed's refusal and the warning that
// others can read the key name the modes Pisati takes, for a credential
// as for a file.
func cannotRead(err error, where string, named namedKind) error {
	reason := pathless(err)
	above, it := "every directory above it", "the file"
	if named == aCredential {
		above, it = "the directory that CREDENTIALS_DIRECTORY names", "the credential"
	}
	switch {
	case errors.Is(err, fs.ErrPermission):
		return explain.New(reason, "Pisati cannot read %s, because the operating system does not let the user that "+
			"runs Pisati read it. That user needs permission to read %s, and to enter %s", where, it, above)
	case errors.Is(err, fs.ErrNotExist) && named == aCredential:
		return explain.New(reason, "Pisati cannot read %s, because the directory that CREDENTIALS_DIRECTORY names "+
			"holds no credential of that name", where)
	case errors.Is(err, fs.ErrNotExist):
		return explain.New(reason, "Pisati cannot read %s, because there is no file at that path. Check that the "+
			"path names the file that holds the main key", where)
	case errors.Is(err, syscall.ENOTDIR):
		return explain.New(reason, "Pisati cannot read %s, because a part of its path that should be a directory is "+
			"a file. Check the path", where)
	}
	return explain.New(reason, "Pisati cannot read %s: %s. Check that the path names %s, and that the user that "+
		"runs Pisati can read it", where, reason, it)
}

// loadGenerated loads the key Pisati keeps at <state>/keys/main.key,
// generating it first if there is none.
func loadGenerated(ctx context.Context, state *os.Root, log *slog.Logger) (MainKey, error) {
	if state == nil {
		return MainKey{}, explain.New(ErrInvalid, "crypto.LoadMainKey was given no state directory to keep a generated "+
			"main key in, %s", bug)
	}
	dirPath := filepath.Join(state.Name(), keysDir)
	keyPath := filepath.Join(dirPath, keyName)
	name := filepath.Join(keysDir, keyName)
	where := "the main key file " + keyPath
	if err := ctx.Err(); err != nil {
		return MainKey{}, explain.Wrap(err, "Pisati stopped before it read %s", where)
	}
	if err := keysDirectory(state, dirPath); err != nil {
		return MainKey{}, err
	}
	fi, err := state.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := ctx.Err(); err != nil {
			return MainKey{}, explain.Wrap(err, "Pisati stopped before it made %s", where)
		}
		return generate(ctx, state, name, keyPath, log)
	case err != nil:
		return MainKey{}, explain.New(pathless(err), "Pisati cannot check %s: %s. Check that the user that runs Pisati "+
			"can enter the directory %s", where, pathless(err), dirPath)
	}
	if err := checkGenerated(fi, keyPath); err != nil {
		return MainKey{}, err
	}
	if err := ctx.Err(); err != nil {
		return MainKey{}, explain.Wrap(err, "Pisati stopped before it read %s", where)
	}
	f, err := state.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return MainKey{}, explain.New(pathless(err), "Pisati cannot read %s: %s. Check that the user that runs Pisati "+
			"owns the file and may read it", where, pathless(err))
	}
	key, err := readGenerated(f, fi, where)
	defer clear(key[:])
	if err := errors.Join(err, f.Close()); err != nil {
		return MainKey{}, err
	}
	return newMainKey(key), nil
}

// keysDirectory makes the keys directory in state, mode 0700, when there
// is none, and returns an error wrapping ErrKeyFile unless it is a
// directory, not a symbolic link, that others cannot use. Its group may,
// and it may be setgid, as fsGroup makes it.
func keysDirectory(state *os.Root, dirPath string) error {
	fi, err := state.Lstat(keysDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := state.Mkdir(keysDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return explain.New(pathless(err), "Pisati cannot make the directory %s, for the main key it generates: %s. "+
				"Check that the user that runs Pisati can write to the state directory %s", dirPath, pathless(err),
				state.Name())
		}
		fi, err = state.Lstat(keysDir)
	}
	if err != nil {
		return explain.New(pathless(err), "Pisati cannot check the directory %s, which holds the main key it generates: "+
			"%s. Check that the user that runs Pisati can enter the state directory %s", dirPath, pathless(err),
			state.Name())
	}
	switch mode := fi.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because it is a symbolic link. Pisati makes this "+
			"directory itself, with mode 0700, for the main key it generates: put a directory in the link's place",
			dirPath)
	case !mode.IsDir():
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because it is not a directory. Pisati keeps the main key "+
			"it generates in a directory of this name, with mode 0700: move what is there to another place", dirPath)
	case mode.Perm()&0o007 != 0:
		return explain.New(ErrKeyFile, "Pisati cannot use the directory %s, because its mode, %04o, lets users other "+
			"than its owner and its group use it. Run: chmod 0700 %s", dirPath, mode.Perm(), dirPath)
	}
	return nil
}

// checkGenerated returns an error wrapping ErrKeyFile unless fi, from
// Lstat, is a regular file, not a symbolic link, that others cannot use,
// of a key file's size. Its group may read and write it, as fsGroup makes
// it.
func checkGenerated(fi fs.FileInfo, keyPath string) error {
	where := "the main key file " + keyPath
	switch mode := fi.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because it is a symbolic link. Pisati generated this key, "+
			"and never makes a link to it: put the key file itself in the link's place, with mode 0600, or set "+
			"keys.main_key_file (PISATI_KEYS_MAIN_KEY_FILE) to the file's path", where)
	case !mode.IsRegular():
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because it is not a regular file. Restore the key file "+
			"that was backed up with the database", where)
	case mode.Perm()&0o007 != 0:
		return explain.New(ErrKeyFile, "Pisati cannot use %s, because its mode, %04o, lets users other than its owner "+
			"and its group use it. Run: chmod 0600 %s", where, mode.Perm(), keyPath)
	}
	return checkSize(fi.Size(), where)
}

// readGenerated reads the key from f, the generated key file, which must
// still be the file checked, checked as fi.
func readGenerated(f *os.File, checked fs.FileInfo, where string) ([keySize]byte, error) {
	opened, err := f.Stat()
	if err != nil {
		return [keySize]byte{}, explain.New(pathless(err), "Pisati cannot read %s: %s. The operating system could "+
			"not report on the file it had open, which points to a fault of the disk or file system that holds it: "+
			"the system log may say more", where, pathless(err))
	}
	// os.Root follows a link inside the root, so a link put in the
	// file's place since it was checked would be opened: the file opened
	// must be the one checked.
	if !os.SameFile(checked, opened) {
		return [keySize]byte{}, explain.New(ErrKeyFile, "Pisati cannot use %s, because it was replaced while Pisati read "+
			"it. Only Pisati should write this file", where)
	}
	return readKey(f, where, aFile)
}

// generate generates a key into the file name in state, keyPath, which
// does not exist, and logs that it did.
func generate(ctx context.Context, state *os.Root, name, keyPath string, log *slog.Logger) (MainKey, error) {
	var key [keySize]byte
	defer clear(key[:])
	if _, err := rand.Read(key[:]); err != nil {
		return MainKey{}, explain.Wrap(err, "Pisati cannot get random bytes for a new main key")
	}
	text := append(base64.StdEncoding.AppendEncode(nil, key[:]), '\n')
	defer clear(text)
	if err := writeKey(state, name, text); err != nil {
		return MainKey{}, explain.Wrap(err, "Pisati cannot write the main key it generated to %s", keyPath)
	}
	log.WarnContext(ctx, "main key generated on the data volume", "path", keyPath, "action", generatedAction)
	return newMainKey(key), nil
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

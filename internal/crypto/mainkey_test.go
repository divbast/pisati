// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
)

// wantSameMainKey fails the test unless got is the main key want is: a
// keyring opened with want opens with got too.
func wantSameMainKey(t *testing.T, want, got crypto.MainKey) {
	t.Helper()
	s := newStore(t)
	openKeyring(t, s, want)
	if _, err := crypto.OpenKeyring(t.Context(), s, got, clock, rand.Reader); err != nil {
		t.Fatalf("the main key loaded is not the one written: %v", err)
	}
}

func TestGeneratedMainKeyAndWarning(t *testing.T) {
	dir := t.TempDir()
	state, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	}()
	log, rec := newLogger()
	first, err := crypto.LoadMainKey(t.Context(), config.Generated{}, nil, state, log)
	if err != nil {
		t.Fatalf("LoadMainKey(Generated): %v", err)
	}

	keysDir := filepath.Join(dir, "keys")
	keyPath := filepath.Join(keysDir, "main.key")
	keyName := filepath.Join("keys", "main.key")
	for _, f := range []struct {
		path string
		mode fs.FileMode
	}{{keysDir, fs.ModeDir | 0o700}, {keyPath, 0o600}} {
		fi, err := os.Lstat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != f.mode {
			t.Errorf("%s: mode %v, want %v", f.path, fi.Mode(), f.mode)
		}
	}
	text, err := state.ReadFile(keyName)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) != 45 || text[44] != '\n' {
		t.Fatalf("the key file holds %d bytes, want 44 characters of base64 and a newline", len(text))
	}
	if key, err := base64.StdEncoding.Strict().DecodeString(string(text[:44])); err != nil || len(key) != 32 {
		t.Fatalf("the key file holds no 32-byte key in base64 (%d bytes): %v", len(key), err)
	}

	records := rec.all()
	if len(records) != 1 {
		t.Fatalf("%d records logged, want one warning", len(records))
	}
	w := records[0]
	if w.level != slog.LevelWarn || !strings.Contains(w.msg, "data volume") || w.attrs["path"] != keyPath {
		t.Errorf("logged %s %q %v; want a warning that the key is on the data volume, naming %s", w.level, w.msg, w.attrs, keyPath)
	}
	if action := strings.ToLower(w.attrs["action"]); !strings.Contains(action, "back up") || !strings.Contains(action, "restore") ||
		!strings.Contains(action, "database") {
		t.Errorf("the warning's action %q does not say to back the key up, and restore it, with the database", action)
	}
	if rec.mentions(string(text[:44])) {
		t.Error("the warning shows the key")
	}

	// A later start reads the key it generated, changes nothing, and does
	// not warn again.
	second, err := crypto.LoadMainKey(t.Context(), config.Generated{}, nil, state, log)
	if err != nil {
		t.Fatalf("LoadMainKey(Generated) again: %v", err)
	}
	if got := len(rec.all()); got != 1 {
		t.Errorf("%d records after the second load, want the one warning of the first", got)
	}
	again, err := state.ReadFile(keyName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, text) {
		t.Error("the second load changed the key file")
	}
	wantSameMainKey(t, first, second)
	// The generated file is a key file an operator may name too.
	named, err := fromFile(t, keyPath, discard())
	if err != nil {
		t.Fatalf("LoadMainKey(%s): %v", keyPath, err)
	}
	wantSameMainKey(t, first, named)
}

// TestGeneratedMainKeySharedWithGroup loads the generated key from a
// volume its group shares, as Kubernetes mounts one with fsGroup: each
// mount, under the default fsGroupChangePolicy, adds read and write for
// the group to every file, and read, write, search and setgid to every
// directory.
func TestGeneratedMainKeySharedWithGroup(t *testing.T) {
	for _, c := range []struct {
		name          string
		dir, file     fs.FileMode
		makeDirBefore bool
	}{
		{name: "fsGroup", dir: fs.ModeSetgid | 0o770, file: 0o660},
		{name: "group read", dir: 0o750, file: 0o640},
		{name: "group write", dir: 0o730, file: 0o620},
		{name: "setgid alone", dir: fs.ModeSetgid | 0o700, file: 0o600},
		{name: "generated into a directory the group shares", dir: fs.ModeSetgid | 0o770, file: 0o600, makeDirBefore: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			keysDir := filepath.Join(dir, "keys")
			if c.makeDirBefore {
				if err := os.Mkdir(keysDir, 0o700); err != nil {
					t.Fatal(err)
				}
				chmod(t, keysDir, c.dir)
			}
			state, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := state.Close(); err != nil {
					t.Error(err)
				}
			}()
			first, err := crypto.LoadMainKey(t.Context(), config.Generated{}, nil, state, discard())
			if err != nil {
				t.Fatalf("LoadMainKey(Generated): %v", err)
			}
			keyPath := filepath.Join(keysDir, "main.key")
			fi, err := os.Lstat(keyPath)
			if err != nil {
				t.Fatalf("the key file LoadMainKey made: %v", err)
			}
			if fi.Mode() != 0o600 {
				t.Fatalf("the key file was made with mode %v, want 0600 whatever its directory's", fi.Mode())
			}
			chmod(t, keysDir, c.dir)
			chmod(t, keyPath, c.file)

			log, rec := newLogger()
			again, err := crypto.LoadMainKey(t.Context(), config.Generated{}, nil, state, log)
			if err != nil {
				t.Fatalf("LoadMainKey(Generated) with the directory %v and the file %v: %v", c.dir, c.file, err)
			}
			if n := len(rec.all()); n != 0 {
				t.Errorf("%d records logged, want none", n)
			}
			wantSameMainKey(t, first, again)
		})
	}
}

// chmod sets the mode of the file at path to exactly mode, its setgid bit
// included, failing the test unless the mode it then has is mode.
func chmod(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() &^ fs.ModeType; got != mode {
		t.Fatalf("%s: mode %v after chmod, want %v", path, got, mode)
	}
}

// operatorSource is a way an operator names the key file: by its path, or
// as a systemd credential.
type operatorSource struct {
	name string
	// load loads the main key from the file called file in dir.
	load func(t *testing.T, dir, file string, log *slog.Logger) (crypto.MainKey, error)
	// named returns the attribute a warning names the file by, and its
	// value, which an error names it by too.
	named func(dir, file string) (key, value string)
}

func operatorSources() []operatorSource {
	return []operatorSource{
		{
			name: "file",
			load: func(t *testing.T, dir, file string, log *slog.Logger) (crypto.MainKey, error) {
				return fromFile(t, filepath.Join(dir, file), log)
			},
			named: func(dir, file string) (string, string) { return "path", filepath.Join(dir, file) },
		},
		{
			name: "credential",
			load: func(t *testing.T, dir, file string, log *slog.Logger) (crypto.MainKey, error) {
				return crypto.LoadMainKey(t.Context(), config.FromCredential{Name: file}, os.DirFS(dir), nil, log)
			},
			named: func(_, file string) (string, string) { return "credential", file },
		},
	}
}

// keyFile is a key file for a test to give LoadMainKey: make writes it as
// file in dir, holding key in some form, with whatever else it needs.
type keyFile struct {
	name string
	make func(t *testing.T, dir, file string, key []byte)
}

// holding returns a keyFile that holds data, made from the key, at mode.
func holding(name string, data func(key []byte) []byte, mode os.FileMode) keyFile {
	return keyFile{name: name, make: func(t *testing.T, dir, file string, key []byte) {
		writeFile(t, dir, file, data(key), mode)
	}}
}

// linkedTo returns a keyFile that is a symbolic link to a file elsewhere,
// holding the key at mode.
func linkedTo(name string, mode os.FileMode) keyFile {
	return keyFile{name: name, make: func(t *testing.T, dir, file string, key []byte) {
		target := writeFile(t, t.TempDir(), "real.key", keyText(key), mode)
		if err := os.Symlink(target, filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}}
}

// asIs is the key file's usual content: the key in base64 and a newline.
func asIs(key []byte) []byte { return keyText(key) }

// testAccepts runs LoadMainKey from src on each key file an operator may
// give, and checks it loads the key, warning only when others can read
// the file.
func testAccepts(t *testing.T, src operatorSource) {
	t.Helper()
	for _, c := range []struct {
		keyFile
		warns bool
	}{
		{keyFile: holding("owner read", asIs, 0o400)},
		{keyFile: holding("owner read and write", asIs, 0o600)},
		{keyFile: holding("group read", asIs, 0o440)},
		{keyFile: holding("no newline", func(key []byte) []byte { return keyText(key)[:44] }, 0o400)},
		{keyFile: linkedTo("a symbolic link", 0o400)},
		{keyFile: holding("others read", asIs, 0o444), warns: true},
		{keyFile: linkedTo("a symbolic link to a file others read", 0o644), warns: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			want, key := newMainKeyBytes(t)
			dir := t.TempDir()
			const file = "pisati-main-key"
			c.make(t, dir, file, key)
			log, rec := newLogger()
			got, err := src.load(t, dir, file, log)
			if err != nil {
				t.Fatalf("LoadMainKey: %v", err)
			}
			wantSameMainKey(t, want, got)

			records := rec.all()
			if !c.warns {
				if len(records) != 0 {
					t.Errorf("%d records logged, want none", len(records))
				}
				return
			}
			attr, value := src.named(dir, file)
			if len(records) != 1 {
				t.Fatalf("%d records logged, want one warning", len(records))
			}
			w := records[0]
			if w.level != slog.LevelWarn || w.attrs[attr] != value {
				t.Errorf("logged %s %q %v; want a warning naming %s %s", w.level, w.msg, w.attrs, attr, value)
			}
			if fix := w.attrs["fix"]; !strings.Contains(fix, "0400") || !strings.Contains(fix, "0440") {
				t.Errorf("the warning's fix %q does not give mode 0400 or 0440", fix)
			}
			if rec.mentions(string(keyText(key)[:44])) {
				t.Error("the warning shows the key")
			}
		})
	}
}

func TestMainKeyFromFile(t *testing.T) {
	testAccepts(t, operatorSources()[0])
}

func TestMainKeyFromCredential(t *testing.T) {
	testAccepts(t, operatorSources()[1])

	t.Run("no credentials directory", func(t *testing.T) {
		_, err := crypto.LoadMainKey(t.Context(), config.FromCredential{Name: "pisati-main-key"}, nil, nil, discard())
		if !errors.Is(err, crypto.ErrInvalid) {
			t.Errorf("LoadMainKey with no credentials: %v, want ErrInvalid", err)
		}
	})
	t.Run("a name that is no file's", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"", "../pisati-main-key", "/pisati-main-key", "a/../b"} {
			_, err := crypto.LoadMainKey(t.Context(), config.FromCredential{Name: name}, os.DirFS(dir), nil, discard())
			if !errors.Is(err, crypto.ErrInvalid) {
				t.Errorf("LoadMainKey(credential %q): %v, want ErrInvalid", name, err)
			}
		}
	})
}

func TestLoadMainKeyRefusesWhatItCannotUse(t *testing.T) {
	t.Run("no source", func(t *testing.T) {
		if _, err := crypto.LoadMainKey(t.Context(), nil, nil, nil, discard()); !errors.Is(err, crypto.ErrInvalid) {
			t.Errorf("LoadMainKey(nil): %v, want ErrInvalid", err)
		}
	})
	t.Run("no logger", func(t *testing.T) {
		path := writeFile(t, t.TempDir(), "main.key", keyText(randomKey(t)), 0o400)
		if _, err := crypto.LoadMainKey(t.Context(), config.FromFile{Path: path}, nil, nil, nil); !errors.Is(err, crypto.ErrInvalid) {
			t.Errorf("LoadMainKey without a logger: %v, want ErrInvalid", err)
		}
	})
	t.Run("no state directory", func(t *testing.T) {
		if _, err := crypto.LoadMainKey(t.Context(), config.Generated{}, nil, nil, discard()); !errors.Is(err, crypto.ErrInvalid) {
			t.Errorf("LoadMainKey(Generated) without a state directory: %v, want ErrInvalid", err)
		}
	})
	t.Run("an ended context", func(t *testing.T) {
		path := writeFile(t, t.TempDir(), "main.key", keyText(randomKey(t)), 0o400)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := crypto.LoadMainKey(ctx, config.FromFile{Path: path}, nil, nil, discard()); !errors.Is(err, context.Canceled) {
			t.Errorf("LoadMainKey with an ended context: %v, want context.Canceled", err)
		}
		state, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := state.Close(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := crypto.LoadMainKey(ctx, config.Generated{}, nil, state, discard()); !errors.Is(err, context.Canceled) {
			t.Errorf("LoadMainKey(Generated) with an ended context: %v, want context.Canceled", err)
		}
		if _, err := state.Lstat("keys"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("LoadMainKey with an ended context made the keys directory: %v", err)
		}
	})
}

// cleanKeyPath returns a key's base64 that is also an absolute, clean
// path, as a key pasted in place of its file's path might be.
func cleanKeyPath(t *testing.T) string {
	t.Helper()
	for range 1000 {
		text := []byte(base64.StdEncoding.EncodeToString(randomKey(t)))
		// The first character carries the key's first six bits, and
		// any is base64: with '/' there, the text is an absolute path.
		text[0] = '/'
		if p := string(text); filepath.Clean(p) == p {
			return p
		}
	}
	t.Fatal("no key in 1000 reads as a clean path")
	return ""
}

func TestRefusesBadKeyFiles(t *testing.T) {
	stars := []byte(strings.Repeat("*", 44) + "\n")
	for _, src := range operatorSources() {
		t.Run(src.name, func(t *testing.T) {
			for _, c := range []struct {
				keyFile
				want error
				// fix is what the error must say to do.
				fix string
			}{
				{holding("group can write", asIs, 0o460), crypto.ErrKeyFile, "0400"},
				{holding("others can write", asIs, 0o402), crypto.ErrKeyFile, "0400"},
				{holding("anyone can write", asIs, 0o666), crypto.ErrKeyFile, "0400"},
				{linkedTo("a symbolic link to a file the group can write", 0o620), crypto.ErrKeyFile, "0400"},
				{keyFile{"a directory", func(t *testing.T, dir, file string, _ []byte) {
					if err := os.Mkdir(filepath.Join(dir, file), 0o700); err != nil {
						t.Fatal(err)
					}
				}}, crypto.ErrKeyFile, "regular file"},
				{keyFile{"a named pipe", func(t *testing.T, dir, file string, _ []byte) {
					if err := syscall.Mkfifo(filepath.Join(dir, file), 0o400); err != nil {
						t.Fatal(err)
					}
				}}, crypto.ErrKeyFile, "regular file"},
				{holding("empty", func([]byte) []byte { return nil }, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("a character short", func(key []byte) []byte { return keyText(key)[:43] }, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("two newlines", func(key []byte) []byte { return append(keyText(key), '\n') }, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("a carriage return", func(key []byte) []byte {
					return append(keyText(key)[:44], '\r', '\n')
				}, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("a space for the newline", func(key []byte) []byte {
					return append(keyText(key)[:44], ' ')
				}, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("the raw key", slices.Clone[[]byte], 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("not base64", func([]byte) []byte { return stars }, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("33 bytes", func(key []byte) []byte {
					return []byte(base64.StdEncoding.EncodeToString(append(key, 7)) + "\n")
				}, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("a newline inside", func(key []byte) []byte {
					text := keyText(key)
					return slices.Concat(text[:20], []byte("\n"), text[20:43])
				}, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{holding("unused bits set", func(key []byte) []byte {
					text := keyText(key)
					// The character before the padding carries two bits
					// the key has no use for, which must be zero.
					text[42] = base64Alphabet[strings.IndexByte(base64Alphabet, text[42])|1]
					return text
				}, 0o400), crypto.ErrKeyFile, "openssl rand -base64 32"},
				{keyFile{"missing", func(*testing.T, string, string, []byte) {}}, fs.ErrNotExist, ""},
			} {
				t.Run(c.name, func(t *testing.T) {
					dir := t.TempDir()
					const file = "pisati-main-key"
					key := randomKey(t)
					c.make(t, dir, file, key)
					log, rec := newLogger()
					_, err := src.load(t, dir, file, log)
					if !errors.Is(err, c.want) {
						t.Fatalf("LoadMainKey: %v, want %v", err, c.want)
					}
					_, named := src.named(dir, file)
					if msg := err.Error(); !strings.Contains(msg, named) || !strings.Contains(msg, c.fix) {
						t.Errorf("LoadMainKey: %q; want it to name %s and say %q", msg, named, c.fix)
					}
					text := string(keyText(key)[:44])
					if msg := err.Error(); strings.Contains(msg, text[:20]) || strings.Contains(msg, "****") {
						t.Error("the error shows what the file holds")
					}
					if n := len(rec.all()); n != 0 {
						t.Errorf("%d records logged for a file refused, want none", n)
					}
				})
			}
		})
	}

	t.Run("file path", func(t *testing.T) {
		key := cleanKeyPath(t)
		for _, c := range []struct{ name, path string }{
			{"relative", "secrets/main.key"},
			{"not clean", "/run/secrets/../secrets/main.key"},
			{"the key itself", key},
		} {
			t.Run(c.name, func(t *testing.T) {
				_, err := fromFile(t, c.path, discard())
				if !errors.Is(err, crypto.ErrInvalid) {
					t.Fatalf("LoadMainKey(%s): %v, want ErrInvalid", c.name, err)
				}
				if strings.Contains(err.Error(), c.path) {
					t.Error("the error repeats the path, which may be the key itself")
				}
			})
		}
	})

	t.Run("generated", func(t *testing.T) {
		// keyDir makes the keys directory at mode.
		keyDir := func(t *testing.T, state string, mode os.FileMode) string {
			t.Helper()
			dir := filepath.Join(state, "keys")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			chmod(t, dir, mode)
			return dir
		}
		goodKey := func(t *testing.T, dir string, key []byte, mode os.FileMode) {
			t.Helper()
			writeFile(t, dir, "main.key", keyText(key), mode)
		}
		for _, c := range []struct {
			name string
			make func(t *testing.T, state string, key []byte)
			// named is the path, in the state directory, the error names.
			named, fix string
		}{
			{"the key file a symbolic link", func(t *testing.T, state string, key []byte) {
				dir := keyDir(t, state, 0o700)
				writeFile(t, state, "elsewhere.key", keyText(key), 0o600)
				if err := os.Symlink("../elsewhere.key", filepath.Join(dir, "main.key")); err != nil {
					t.Fatal(err)
				}
			}, "keys/main.key", "symbolic link"},
			{"the key file readable by others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, 0o700), key, 0o604)
			}, "keys/main.key", "chmod 0600"},
			{"the key file writable by others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, 0o700), key, 0o602)
			}, "keys/main.key", "chmod 0600"},
			{"the key file executable by others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, 0o700), key, 0o601)
			}, "keys/main.key", "chmod 0600"},
			{"the key file the group shares readable by others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, fs.ModeSetgid|0o770), key, 0o664)
			}, "keys/main.key", "chmod 0600"},
			{"the key file a directory", func(t *testing.T, state string, _ []byte) {
				if err := os.Mkdir(filepath.Join(keyDir(t, state, 0o700), "main.key"), 0o700); err != nil {
					t.Fatal(err)
				}
			}, "keys/main.key", "regular file"},
			{"the key file a character short", func(t *testing.T, state string, key []byte) {
				writeFile(t, keyDir(t, state, 0o700), "main.key", keyText(key)[:43], 0o600)
			}, "keys/main.key", "openssl rand -base64 32"},
			{"the key file not base64", func(t *testing.T, state string, _ []byte) {
				writeFile(t, keyDir(t, state, 0o700), "main.key", stars, 0o600)
			}, "keys/main.key", "openssl rand -base64 32"},
			{"the keys directory a symbolic link", func(t *testing.T, state string, key []byte) {
				elsewhere := filepath.Join(state, "elsewhere")
				if err := os.Mkdir(elsewhere, 0o700); err != nil {
					t.Fatal(err)
				}
				goodKey(t, elsewhere, key, 0o600)
				if err := os.Symlink("elsewhere", filepath.Join(state, "keys")); err != nil {
					t.Fatal(err)
				}
			}, "keys", "symbolic link"},
			{"the keys directory the group shares open to others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, fs.ModeSetgid|0o775), key, 0o660)
			}, "keys", "chmod 0700"},
			{"the keys directory writable by others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, 0o702), key, 0o600)
			}, "keys", "chmod 0700"},
			{"the keys directory readable by others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, 0o704), key, 0o600)
			}, "keys", "chmod 0700"},
			{"the keys directory open to others", func(t *testing.T, state string, key []byte) {
				goodKey(t, keyDir(t, state, 0o701), key, 0o600)
			}, "keys", "chmod 0700"},
			{"the keys directory a file", func(t *testing.T, state string, key []byte) {
				writeFile(t, state, "keys", keyText(key), 0o600)
			}, "keys", "directory"},
		} {
			t.Run(c.name, func(t *testing.T) {
				dir := t.TempDir()
				key := randomKey(t)
				c.make(t, dir, key)
				before := snapshot(t, dir)
				state, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := state.Close(); err != nil {
						t.Error(err)
					}
				}()
				log, rec := newLogger()
				_, err = crypto.LoadMainKey(t.Context(), config.Generated{}, nil, state, log)
				if !errors.Is(err, crypto.ErrKeyFile) {
					t.Fatalf("LoadMainKey(Generated): %v, want ErrKeyFile", err)
				}
				named := filepath.Join(dir, filepath.FromSlash(c.named))
				if msg := err.Error(); !strings.Contains(msg, named) || !strings.Contains(msg, c.fix) {
					t.Errorf("LoadMainKey(Generated): %q; want it to name %s and say %q", msg, named, c.fix)
				}
				if strings.Contains(err.Error(), string(keyText(key)[:20])) || strings.Contains(err.Error(), "****") {
					t.Error("the error shows what the file holds")
				}
				if n := len(rec.all()); n != 0 {
					t.Errorf("%d records logged for a key refused, want none", n)
				}
				if after := snapshot(t, dir); !maps.Equal(before, after) {
					t.Errorf("LoadMainKey(Generated) changed the state directory: %v, was %v", after, before)
				}
			})
		}
	})
}

// base64Alphabet is standard base64's alphabet, each character at its
// value.
const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// snapshot returns what each entry under dir is, a symbolic link read
// rather than followed: its mode, and its target or a hash of what it
// holds.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	entries := map[string]string{}
	err = fs.WalkDir(root.FS(), ".", func(name string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := root.Lstat(name)
		if err != nil {
			return err
		}
		what := fi.Mode().String()
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := root.Readlink(name)
			if err != nil {
				return err
			}
			what += " -> " + target
		case fi.Mode().IsRegular():
			data, err := root.ReadFile(name)
			if err != nil {
				return err
			}
			what += fmt.Sprintf(" %d bytes, %x", len(data), sha256.Sum256(data))
		}
		entries[name] = what
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// decimal returns b as fmt prints a byte slice, without the brackets: each
// byte in decimal, separated by spaces.
func decimal(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = strconv.Itoa(int(c))
	}
	return strings.Join(parts, " ")
}

func TestMainKeyRedacts(t *testing.T) {
	m, key := newMainKeyBytes(t)
	text := string(keyText(key)[:44])
	leaks := map[string]string{
		"base64":        text,
		"base64url":     base64.RawURLEncoding.EncodeToString(key),
		"hex":           hex.EncodeToString(key),
		"HEX":           strings.ToUpper(hex.EncodeToString(key)),
		"raw":           string(key),
		"decimal bytes": decimal(key),
	}

	type exported struct{ M crypto.MainKey }
	type unexported struct{ m crypto.MainKey }
	var shown []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
		shown = append(shown,
			fmt.Sprintf(verb, m),
			fmt.Sprintf(verb, &m),
			fmt.Sprintf(verb, exported{m}),
			fmt.Sprintf(verb, unexported{m}),
			fmt.Sprintf(verb, []crypto.MainKey{m}))
	}
	shown = append(shown, m.String(), fmt.Sprint(m), fmt.Sprintln(m))
	text2, err := m.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	shown = append(shown, string(text2))
	for _, v := range []any{m, exported{m}, map[string]crypto.MainKey{"m": m}} {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		shown = append(shown, string(j))
	}
	var logged bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&logged, nil), slog.NewTextHandler(&logged, nil)} {
		slog.New(h).Info("main key", "main", m, "within", exported{m}, "any", slog.AnyValue(m))
	}
	shown = append(shown, logged.String())

	for _, s := range shown {
		for form, leak := range leaks {
			if strings.Contains(s, leak) {
				t.Errorf("a main key shown as %q holds the key, in %s", s[:min(len(s), 12)]+"…", form)
			}
		}
	}
	if !strings.Contains(m.String(), "[redacted]") {
		t.Errorf("String() = %q, want it to say [redacted]", m.String())
	}
	if reflect.TypeFor[crypto.MainKey]().Comparable() {
		t.Error("main keys compare with ==, which would compare them in variable time")
	}
}

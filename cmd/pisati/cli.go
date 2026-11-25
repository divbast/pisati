// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/crypto"
	"github.com/divbast/pisati/internal/store"
)

// Host is everything run takes from the process it runs in. main builds it
// once, from the process itself; a test builds one of its own.
type Host struct {
	// Args are the command line's arguments, after the program's name.
	Args []string
	// Lookup looks up one environment variable, as os.LookupEnv does.
	Lookup func(string) (string, bool)
	// EnvNames names every environment variable set.
	EnvNames []string
	// Clock reads the time, as time.Now does. It must be safe for
	// concurrent use.
	Clock func() time.Time
	// Rand is the random source, crypto/rand's Reader. It must be safe for
	// concurrent use.
	Rand io.Reader
	// Stdout takes a command's result, and serve's log. Stderr takes the
	// other commands' logs, and the errors that end a command.
	Stdout, Stderr io.Writer
	// Halt is closed when a second SIGTERM or SIGINT arrives: run's ctx
	// ends at the first. serve then stops at once, without finishing its
	// drain. Nil means it never closes.
	Halt <-chan struct{}
}

// The codes run exits with.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// defaultConfigDir is the configuration directory when
// $CONFIGURATION_DIRECTORY is not set.
const defaultConfigDir = "/etc/pisati"

// description is the help's description of the program.
const description = "Pisati serves a blog. These commands run it, and look after its database and its keys."

// cli is the command line: one field for each command, each with a Run
// method that kong calls with the context and the Host.
type cli struct {
	Serve   serveCmd   `cmd:"" help:"Serve the blog on server.listen, and /healthz and /metrics on server.internal_listen. Keep running until a SIGTERM or SIGINT signal arrives."`
	Migrate migrateCmd `cmd:"" help:"Update the database's schema to the one this Pisati uses. Print the schema's number before and after, as: schema <before> -> <after>."`
	Backup  backupCmd  `cmd:"" help:"Copy the database to a new file. pisati serve can keep running. The copy does not hold the main key: keep a copy of the main key too, because the database cannot be read without it."`
	Restore restoreCmd `cmd:"" help:"Replace the database with a copy that pisati backup made. Stop pisati serve first. Pisati then needs the main key that was in use when the copy was made."`
	Keys    keysCmd    `cmd:"" help:"Change the encryption keys: the main key, and the data keys that the main key encrypts. This version of Pisati does not yet encrypt any other data with them."`
	Health  healthCmd  `cmd:"" help:"Send GET /healthz to the running pisati serve, on server.internal_listen. Print nothing and exit 0 when it answers 200, which means that it runs and its database answers. Otherwise print why, and exit 1."`
	Version versionCmd `cmd:"" help:"Print the version of Pisati."`
}

// errReported is the error of a command that has already reported why it
// ended, in its log or on stderr: run exits 1, and writes nothing more.
var errReported = errors.New("reported")

// run runs the command h.Args names, and returns the code to exit with:
// exitOK, exitFailed with the reasons on h.Stderr, or exitUsage when kong
// refuses the command line, with kong's error and usage on h.Stderr.
func run(ctx context.Context, h Host) int {
	// Every file Pisati creates, the database and its journals among them,
	// is its own user's alone, whatever umask it was started with.
	syscall.Umask(0o077)

	exited, code := false, exitOK
	var c cli
	parser, err := kong.New(&c,
		kong.Name("pisati"),
		kong.Description(description),
		kong.Writers(h.Stdout, h.Stderr),
		// Only --help calls it: run returns its code rather than exit.
		kong.Exit(func(n int) { exited, code = true, n }),
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.Bind(h),
	)
	if err != nil {
		// A bug in cli's tags, which every test would show.
		report(h.Stderr, fmt.Errorf("the list of commands built into Pisati is broken, which is a bug in Pisati: %w", err))
		return exitFailed
	}
	cmd, err := parser.Parse(h.Args)
	switch {
	case exited:
		return code
	case err != nil:
		usage(h.Stderr, parser, err)
		return exitUsage
	}
	err = cmd.Run()
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errReported):
	case stopped(ctx, err):
		report(h.Stderr, errors.New("the command stopped before it finished, because a SIGTERM or SIGINT signal arrived"))
	default:
		report(h.Stderr, err)
	}
	return exitFailed
}

// stopped reports whether err is ctx's own error, after ctx has ended: the
// command was stopped by a signal, and nothing else failed.
func stopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

// usage writes kong's error for the command line, err, and the short usage
// of the command it was for.
func usage(w io.Writer, parser *kong.Kong, err error) {
	say(w, "pisati: %s\n", err)
	pe, ok := errors.AsType[*kong.ParseError](err)
	if !ok {
		return
	}
	// kong prints usage to its stdout; on a refused command line it
	// belongs on stderr, with the error.
	parser.Stdout = w
	//nolint:errcheck // it fails only when stderr cannot be written, and nothing is left to report that on
	kong.DefaultShortHelpPrinter(kong.HelpOptions{}, pe.Context)
}

// report writes err to w as the errors that end a command: one line for
// each problem it reports, each starting "pisati: ".
func report(w io.Writer, err error) {
	for _, line := range problems(err) {
		say(w, "pisati: %s\n", line)
	}
}

// problems returns each problem err reports, one a line, each ending with
// a full stop so that it reads as a whole sentence. errors.Join puts each
// error it joins on a line of its own, so independent failures, and each
// problem config.Load finds, are a line each.
func problems(err error) []string {
	var lines []string
	for line := range strings.Lines(err.Error()) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".") {
			line += "."
		}
		lines = append(lines, line)
	}
	return lines
}

// say writes to stderr, w. A failure to write is not reported: stderr is
// where it would be reported.
func say(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format, args...) //nolint:errcheck // stderr itself failed, and there is nowhere left to say so
}

// plainError is an error whose message is Pisati's own, worded for the
// operator, around the error that caused it. errors.Is and errors.AsType
// still find the cause, so a stop by a signal is still seen as one.
type plainError struct {
	msg   string
	cause error
}

func (e *plainError) Error() string { return e.msg }
func (e *plainError) Unwrap() error { return e.cause }

// explain returns cause behind a message of Pisati's own.
func explain(cause error, format string, args ...any) error {
	return &plainError{msg: fmt.Sprintf(format, args...), cause: cause}
}

// configError is the configuration Pisati refuses: every problem
// config.Load found, and every problem with the directories it reads
// from, joined.
type configError struct {
	problems error
}

func (e *configError) Error() string {
	return e.problems.Error()
}

func (e *configError) Unwrap() error {
	return e.problems
}

// loadConfig returns the configuration, read as every command but version
// reads it: from $CONFIGURATION_DIRECTORY, or /etc/pisati, then the
// environment, with $STATE_DIRECTORY and the credentials in
// $CREDENTIALS_DIRECTORY. It returns the credentials directory too, nil
// when there is none. A refused configuration is a *configError holding
// every problem: one with either directory variable among the rest. With
// no usable configuration directory, no setting is reported missing, as
// config does for a file it cannot read: the files there may hold it.
func loadConfig(ctx context.Context, h Host) (config.Config, fs.FS, error) {
	confDir, confErr := directory(h, "CONFIGURATION_DIRECTORY")
	credsDir, credsErr := directory(h, "CREDENTIALS_DIRECTORY")
	var files, creds fs.FS
	if confErr == nil {
		files = os.DirFS(cmp.Or(confDir, defaultConfigDir))
	}
	if credsErr == nil && credsDir != "" {
		creds = os.DirFS(credsDir)
	}
	stateDir, _ := lookup(h, "STATE_DIRECTORY")
	cfg, err := config.Load(ctx, config.Sources{
		Files:       files,
		Env:         h.Lookup,
		EnvNames:    h.EnvNames,
		Credentials: creds,
	}, stateDir)
	if stopped(ctx, err) {
		return config.Config{}, nil, err
	}
	if confErr != nil {
		err = withoutMissing(err)
	}
	if err := errors.Join(confErr, credsErr, err); err != nil {
		return config.Config{}, nil, &configError{problems: err}
	}
	return cfg, creds, nil
}

// withoutMissing returns the problems err joins, but those of a setting
// reported missing.
func withoutMissing(err error) error {
	joined, ok := errors.AsType[interface {
		error
		Unwrap() []error
	}](err)
	if !ok {
		if errors.Is(err, config.ErrMissing) {
			return nil
		}
		return err
	}
	var kept []error
	for _, e := range joined.Unwrap() {
		if !errors.Is(e, config.ErrMissing) {
			kept = append(kept, e)
		}
	}
	return errors.Join(kept...)
}

// lookup looks up the variable name in h's environment.
func lookup(h Host, name string) (string, bool) {
	if h.Lookup == nil {
		return "", false
	}
	return h.Lookup(name)
}

// directory returns the directory the variable name holds, or "" when it
// is not set or empty. A value that is not one absolute, clean path is an
// error naming the variable and the value, which is a path, and no secret.
func directory(h Host, name string) (string, error) {
	v, ok := lookup(h, name)
	if !ok || v == "" {
		return "", nil
	}
	if !filepath.IsAbs(v) || filepath.Clean(v) != v || strings.Contains(v, ":") {
		return "", fmt.Errorf(`%s is set to %q, which Pisati cannot use. Set it to one directory, as a full path that starts with "/", `+
			`with no ":", no "//", no "." or ".." part, and no "/" at the end`, name, v)
	}
	return v, nil
}

// absolute returns path as an absolute path: a relative one is resolved
// against the working directory, as a command line's paths are.
func absolute(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", explain(err, "cannot find the full path of %s, because the current directory cannot be read: %s. "+
			`Give the full path instead, starting with "/"`, path, reason(err))
	}
	return abs, nil
}

// reason returns what the innermost error err wraps says, such as
// "permission denied" or "address already in use": what the operating
// system said, without the call and path around it.
func reason(err error) string {
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err.Error()
		}
		err = inner
	}
}

// state is a state directory under its lock, with its database open and
// migrated.
type state struct {
	dir  string
	lock store.Lock
	db   *store.Store
}

// openState checks that the state directory dir can be written to, takes
// its lock, opens its database and migrates it. It returns the schema
// versions it found and left, as Migrate does. On an error it leaves
// nothing open or held.
func openState(ctx context.Context, dir string) (_ *state, from, to store.SchemaVersion, err error) {
	if err := checkStateDir(dir); err != nil {
		return nil, 0, 0, err
	}
	lock, err := lockState(ctx, dir)
	if err != nil {
		return nil, 0, 0, err
	}
	db, err := store.Open(ctx, store.DatabasePath(dir))
	if err != nil {
		return nil, 0, 0, errors.Join(err, lock.Release())
	}
	st := &state{dir: dir, lock: lock, db: db}
	from, to, err = db.Migrate(ctx, lock)
	if err != nil {
		return nil, from, to, errors.Join(err, st.close())
	}
	return st, from, to, nil
}

// close closes the database, which checkpoints its write-ahead log, and
// then releases the lock.
func (s *state) close() error {
	return errors.Join(s.db.Close(), s.lock.Release())
}

// stateDirSetting names the state directory's setting and its variable,
// for the advice the state directory's errors give.
const stateDirSetting = "storage.state_dir (PISATI_STORAGE_STATE_DIR)"

// checkFile starts the name of the file checkStateDir writes.
const checkFile = ".pisati-write-check-"

// checkStateDir checks that the state directory dir exists and can be
// written to, by making a file in it and removing it again: the one test
// that sees every reason a write can fail, a read-only mount, a mode or
// an access list among them. Each error says what to do about its cause.
func checkStateDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return explain(err, "the state directory %s does not exist. Create it, and give the user that runs Pisati permission "+
			"to write to it. Or set %s to another directory", dir, stateDirSetting)
	case err != nil:
		return explain(err, "cannot read the state directory %s: %s. Set %s to a directory that the user that runs Pisati "+
			"can read and write", dir, reason(err), stateDirSetting)
	case !info.IsDir():
		return fmt.Errorf("the state directory %s is not a directory. Set %s to a directory", dir, stateDirSetting)
	}
	f, err := os.CreateTemp(dir, checkFile+"*")
	switch {
	case errors.Is(err, syscall.EROFS):
		return explain(err, "cannot write to the state directory %s, because it is on a read-only file system. "+
			"Mount a writable volume at %s, or set %s to a writable directory", dir, dir, stateDirSetting)
	case errors.Is(err, fs.ErrPermission):
		return explain(err, "cannot write to the state directory %s, because the user that runs Pisati does not have "+
			"permission to. Give that user permission to write to %s, or set %s to a directory it can write to",
			dir, dir, stateDirSetting)
	case err != nil:
		return explain(err, "cannot write to the state directory %s: %s. Set %s to a directory that the user that runs "+
			"Pisati can write to", dir, reason(err), stateDirSetting)
	}
	name := f.Name()
	if err := errors.Join(f.Close(), os.Remove(name)); err != nil {
		return explain(err, "Pisati wrote a file to the state directory %s to check that it can, but cannot remove it: %s. "+
			"Remove the file whose name starts with %s from %s", dir, reason(err), checkFile, dir)
	}
	return nil
}

// lockState takes the lock on the state directory dir, which a running
// Pisati holds.
func lockState(ctx context.Context, dir string) (store.Lock, error) {
	lock, err := store.AcquireLock(ctx, dir)
	if errors.Is(err, store.ErrLocked) {
		return store.Lock{}, explain(err, "another Pisati process is using the state directory %s. "+
			"Only one Pisati process can use it at a time. Stop the other process, then run this command again", dir)
	}
	return lock, err
}

// keyOrigin is whether the main key Pisati loaded is one it generated on
// this start. The zero keyOrigin is not.
type keyOrigin uint8

const (
	// keyKept is a key that was there before this start, or one the
	// operator gives: it is not generated now.
	keyKept keyOrigin = iota
	// keyGeneratedNow is a key Pisati generated on this start, as there
	// was none at <state>/keys/main.key.
	keyGeneratedNow
)

// generatedKeyPath returns where Pisati keeps the main key it generates
// in the state directory dir.
func generatedKeyPath(dir string) string {
	return filepath.Join(dir, "keys", "main.key")
}

// loadMainKey loads the main key from where src says it is, with the
// state directory dir opened as a root for the key Pisati generates, and
// says whether it was generated on this start. It runs under the state
// directory's lock, so nothing else makes the key meanwhile.
func loadMainKey(ctx context.Context, src config.SecretSource, creds fs.FS, dir string, log *slog.Logger) (crypto.MainKey, keyOrigin, error) {
	origin := keyKept
	if _, ok := src.(config.Generated); ok {
		if _, err := os.Lstat(generatedKeyPath(dir)); errors.Is(err, fs.ErrNotExist) {
			origin = keyGeneratedNow
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return crypto.MainKey{}, keyKept, err
	}
	key, err := crypto.LoadMainKey(ctx, src, creds, root, log)
	if err := errors.Join(err, root.Close()); err != nil {
		return crypto.MainKey{}, keyKept, err
	}
	return key, origin, nil
}

// openKeyring opens the keyring of the database in st with key, the main
// key src gave. A key that does not decrypt the database's data keys is
// reported with mainKeyMismatch.
func openKeyring(ctx context.Context, h Host, st *state, src config.SecretSource, key crypto.MainKey, origin keyOrigin) (*crypto.Keyring, error) {
	k, err := crypto.OpenKeyring(ctx, st.db, key, h.Clock, h.Rand)
	if errors.Is(err, crypto.ErrMainKeyMismatch) {
		return nil, mainKeyMismatch(err, src, st.dir, origin)
	}
	return k, err
}

// mainKeyMismatch is the error, around cause, for a main key that does
// not decrypt the data keys in the database in the state directory dir:
// it says where the key came from, and where to put the right one.
func mainKeyMismatch(cause error, src config.SecretSource, dir string, origin keyOrigin) error {
	const other = "set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the file that holds it"
	switch s := src.(type) {
	case config.Generated:
		path := generatedKeyPath(dir)
		made := "which Pisati created when it first used this state directory"
		if origin == keyGeneratedNow {
			made = "which Pisati created on this start, because there was none"
		}
		return explain(cause, "the main key in %[1]s, %[2]s, cannot decrypt the data keys in this database. "+
			"The database needs the main key it was used with, for example the one backed up with it. "+
			"Put that key at %[1]s, or %[3]s, or put it in the credential pisati-main-key", path, made, other)
	case config.FromFile:
		return explain(cause, "the main key in %[1]s, the file that keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE names, "+
			"cannot decrypt the data keys in this database. The database needs the main key it was used with, "+
			"for example the one backed up with it. Put that key in %[1]s, or %[2]s", s.Path, other)
	case config.FromCredential:
		return explain(cause, "the main key in the credential %[1]s cannot decrypt the data keys in this database. "+
			"The database needs the main key it was used with, for example the one backed up with it. "+
			"Put that key in the credential %[1]s, or %[2]s", s.Name, other)
	default:
		// config.Load returns no other source: a bug.
		return explain(cause, "the main key cannot decrypt the data keys in this database, and it came from a source "+
			"that Pisati does not know, %T, which is a bug in Pisati", src)
	}
}

// versionCmd is pisati version.
type versionCmd struct{}

// Run prints the version.
func (versionCmd) Run(h Host) error {
	_, err := fmt.Fprintf(h.Stdout, "Pisati %s\n", version)
	return err
}

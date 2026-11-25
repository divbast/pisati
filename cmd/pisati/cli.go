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
	// other commands' logs, and the error that ends a command.
	Stdout, Stderr io.Writer
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
const description = "Pisati serves a blog, and manages its database and its keys."

// cli is the command line: one field for each command, each with a Run
// method that kong calls with the context and the Host.
type cli struct {
	Serve   serveCmd   `cmd:"" help:"Serve the blog on server.listen, and the health check and metrics on server.internal_listen. Run until stopped by SIGTERM or SIGINT."`
	Migrate migrateCmd `cmd:"" help:"Update the database to the version this Pisati uses. Print the version before and after."`
	Backup  backupCmd  `cmd:"" help:"Copy the database to a new file. pisati serve can keep running."`
	Restore restoreCmd `cmd:"" help:"Replace the database with a copy made by pisati backup. Stop pisati serve first."`
	Keys    keysCmd    `cmd:"" help:"Change the keys that encrypt the secret data in the database."`
	Health  healthCmd  `cmd:"" help:"Check that pisati serve answers on server.internal_listen. Print nothing when it does."`
	Version versionCmd `cmd:"" help:"Print the version of Pisati."`
}

// errReported is the error of a command that has written its own report
// to stderr: run exits 1, and writes nothing more.
var errReported = errors.New("reported")

// run runs the command h.Args names, and returns the code to exit with:
// exitOK, exitFailed with the reason on h.Stderr, or exitUsage when kong
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
		report(h.Stderr, fmt.Errorf("the command line's grammar is wrong: %w", err))
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
		report(h.Stderr, errors.New("the command stopped before it finished, because Pisati received SIGTERM or SIGINT"))
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

// report writes err to w as the error that ends a command: its first line
// after "pisati: ", and each further line indented under it. A message
// that ends without a full stop is given one, so that the operator reads
// whole sentences.
func report(w io.Writer, err error) {
	msg := err.Error()
	if !strings.HasSuffix(msg, ".") && !strings.HasSuffix(msg, ":") {
		msg += "."
	}
	first, rest, _ := strings.Cut(msg, "\n")
	say(w, "pisati: %s\n", first)
	if rest == "" {
		return
	}
	for line := range strings.SplitSeq(rest, "\n") {
		say(w, "  %s\n", line)
	}
}

// say writes to stderr, w. A failure to write is not reported: stderr is
// where it would be reported.
func say(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format, args...) //nolint:errcheck // stderr itself failed, and there is nowhere left to say so
}

// configError is the configuration config.Load, or the directories it is
// read from, refused: every problem, each on a line of its own.
type configError struct {
	problems error
}

func (e *configError) Error() string {
	return "the configuration has these problems:\n" + e.problems.Error()
}

func (e *configError) Unwrap() error {
	return e.problems
}

// loadConfig returns the configuration, read as every command but version
// reads it: from $CONFIGURATION_DIRECTORY, or /etc/pisati, then the
// environment, with $STATE_DIRECTORY and the credentials in
// $CREDENTIALS_DIRECTORY. It returns the credentials directory too, nil
// when there is none. A refused configuration is a *configError.
func loadConfig(ctx context.Context, h Host) (config.Config, fs.FS, error) {
	confDir, confErr := directory(h, "CONFIGURATION_DIRECTORY")
	credsDir, credsErr := directory(h, "CREDENTIALS_DIRECTORY")
	if err := errors.Join(confErr, credsErr); err != nil {
		return config.Config{}, nil, &configError{problems: err}
	}
	var creds fs.FS
	if credsDir != "" {
		creds = os.DirFS(credsDir)
	}
	stateDir, _ := lookup(h, "STATE_DIRECTORY")
	cfg, err := config.Load(ctx, config.Sources{
		Files:       os.DirFS(cmp.Or(confDir, defaultConfigDir)),
		Env:         h.Lookup,
		EnvNames:    h.EnvNames,
		Credentials: creds,
	}, stateDir)
	switch {
	case err == nil:
		return cfg, creds, nil
	case stopped(ctx, err):
		return config.Config{}, nil, err
	}
	return config.Config{}, nil, &configError{problems: err}
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
		return "", fmt.Errorf(`%s is %q: it must be one absolute path, with no ":", no "//", no "." or ".." part, and no "/" at the end`, name, v)
	}
	return v, nil
}

// absolute returns path as an absolute path: a relative one is resolved
// against the working directory, as a command line's paths are.
func absolute(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("cannot find the working directory, to find the file %s in it: %s", path, reason(err))
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

// checkStateDir checks that the state directory dir exists and can be
// written to, by making a file in it and removing it again: the one test
// that sees every reason a write can fail, a read-only mount, a mode or
// an access list among them.
func checkStateDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("the state directory %s does not exist. Create it and let the user that runs Pisati write to it, "+
			"or set storage.state_dir to another directory", dir)
	case err != nil:
		return fmt.Errorf("cannot read the state directory %s: %s", dir, reason(err))
	case !info.IsDir():
		return fmt.Errorf("the state directory %s is not a directory. Set storage.state_dir to a directory", dir)
	}
	f, err := os.CreateTemp(dir, ".pisati-write-check-*")
	if err != nil {
		return fmt.Errorf("cannot write to the state directory %s: %s. Let the user that runs Pisati write to it", dir, reason(err))
	}
	name := f.Name()
	if err := errors.Join(f.Close(), os.Remove(name)); err != nil {
		return fmt.Errorf("cannot remove the test file %s from the state directory: %s", name, reason(err))
	}
	return nil
}

// lockState takes the lock on the state directory dir, which a running
// Pisati holds.
func lockState(ctx context.Context, dir string) (store.Lock, error) {
	lock, err := store.AcquireLock(ctx, dir)
	if errors.Is(err, store.ErrLocked) {
		return store.Lock{}, fmt.Errorf("another Pisati process is using the state directory %s. "+
			"Only one Pisati process can use it at a time. Stop the other process, then run this command again", dir)
	}
	return lock, err
}

// loadMainKey loads the main key from where src says it is, with the
// state directory dir opened as a root for the key Pisati generates.
func loadMainKey(ctx context.Context, src config.SecretSource, creds fs.FS, dir string, log *slog.Logger) (crypto.MainKey, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return crypto.MainKey{}, err
	}
	key, err := crypto.LoadMainKey(ctx, src, creds, root, log)
	if err := errors.Join(err, root.Close()); err != nil {
		return crypto.MainKey{}, err
	}
	return key, nil
}

// openKeyring opens the keyring of the database in st with key, the main
// key src gave. A key that does not decrypt the database's data keys is
// reported with mainKeyMismatch.
func openKeyring(ctx context.Context, h Host, st *state, src config.SecretSource, key crypto.MainKey) (*crypto.Keyring, error) {
	k, err := crypto.OpenKeyring(ctx, st.db, key, h.Clock, h.Rand)
	if errors.Is(err, crypto.ErrMainKeyMismatch) {
		return nil, mainKeyMismatch(src, st.dir)
	}
	return k, err
}

// mainKeyMismatch is the error for a main key, from src, that does not
// decrypt the data keys in the database in the state directory dir: it
// says where the key came from, and where to put the right one.
func mainKeyMismatch(src config.SecretSource, dir string) error {
	const other = "set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to the file that holds it"
	switch s := src.(type) {
	case config.Generated:
		path := filepath.Join(dir, "keys", "main.key")
		return fmt.Errorf("the main key in %[1]s cannot decrypt the data keys in this database. "+
			"Put the main key that was backed up with this database at %[1]s. "+
			"Or, if you keep that key in another file, set keys.main_key_file or PISATI_KEYS_MAIN_KEY_FILE to that file, "+
			"or put the key in the credential pisati-main-key", path)
	case config.FromFile:
		return fmt.Errorf("the main key in %[1]s cannot decrypt the data keys in this database. "+
			"Put the main key that was backed up with this database in %[1]s, or %[2]s", s.Path, other)
	case config.FromCredential:
		return fmt.Errorf("the main key in the credential %[1]s cannot decrypt the data keys in this database. "+
			"Put the main key that was backed up with this database in the credential %[1]s, or %[2]s", s.Name, other)
	default:
		// config.Load returns no other source: a bug.
		return fmt.Errorf("the main key cannot decrypt the data keys in this database, and it came from a source Pisati does not know, %T", src)
	}
}

// versionCmd is pisati version.
type versionCmd struct{}

// Run prints the version.
func (versionCmd) Run(h Host) error {
	_, err := fmt.Fprintf(h.Stdout, "Pisati %s\n", version)
	return err
}

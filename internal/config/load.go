// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"path"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	// mainFile and dropInDir are read from Sources.Files.
	mainFile  = "pisati.toml"
	dropInDir = "pisati.toml.d"

	// maxFileSize bounds each configuration file, far above any real one.
	maxFileSize = 1 << 20

	// mainKeyCredential is the credential the main key is read from.
	mainKeyCredential = "pisati-main-key"

	defaultStateDir = "/var/lib/pisati"
)

// Sources is where Load reads the configuration from. Its zero value
// gives nothing: no files, no variables and no credentials.
type Sources struct {
	// Files is the configuration directory, holding pisati.toml and
	// pisati.toml.d. Nil means there is none.
	Files fs.FS
	// Env looks up one variable. Nil means none is set.
	Env func(string) (string, bool)
	// EnvNames names every variable set, so that Load refuses those that
	// look like Pisati's but name no setting.
	EnvNames []string
	// Credentials is $CREDENTIALS_DIRECTORY, nil when it is not set.
	Credentials fs.FS
}

// Load returns the configuration src gives, or an error joining every
// problem it found, each naming the setting, file or variable at fault.
//
// Each source overrides the ones before it: the built-in defaults, then
// pisati.toml, then each pisati.toml.d/*.toml in name order, then the
// PISATI_<SECTION>_<KEY> variables. Tables merge, and a list replaces the
// one before it. A missing pisati.toml or pisati.toml.d is no error.
// stateDirEnv is $STATE_DIRECTORY, or empty when it is not set: when set,
// it replaces the default state directory.
//
// The error reports every unknown key in each file, every unknown
// PISATI_ variable, and every value read that its rule refuses. Decoding a
// file stops at its first syntax or type error, so a file with one is
// reported once, at that error, and none of its settings are used; no
// setting is then reported missing, as that file may hold it. A refused
// secret it holds before the error is still reported.
//
// A secret is never given in place: keys.main_key in any source is an
// error. The main key comes from the file keys.main_key_file names,
// else the credential pisati-main-key when Credentials holds it, else
// it is Generated.
//
// An https dev.vite_url needs dev.vite_cert_file and dev.vite_key_file
// both, which are reported missing without it; with an http dev.vite_url,
// or none, Load accepts them and nothing uses them.
//
// Load checks ctx before each file it reads, and stops with ctx's error
// once it is done. On any error, the Config is the zero Config.
func Load(ctx context.Context, src Sources, stateDirEnv string) (Config, error) {
	files, err := readFiles(ctx, src.Files)
	if err != nil {
		return Config{}, err
	}
	merged := files.values
	env := fromEnv(src.Env)
	overlay(&merged, &env)

	errs := files.errs
	for _, name := range UnknownEnv(src.EnvNames) {
		errs = append(errs, fmt.Errorf("%s: %w", name, ErrUnknown))
	}
	cfg, refusedValues, missing := build(merged)
	errs = append(errs, refusedValues...)
	if len(files.unread) == 0 {
		errs = append(errs, missing...)
		errs = append(errs, missingCertificate(merged.Dev, cfg.Dev)...)
	}

	// systemd's $STATE_DIRECTORY replaces the default, but not a value a
	// source gives.
	if merged.Storage.StateDir == nil && stateDirEnv != "" {
		if dir, err := stateDirectory(stateDirEnv); err != nil {
			errs = append(errs, fmt.Errorf("STATE_DIRECTORY: %w", err))
		} else {
			cfg.Storage.StateDir = dir
		}
	}
	if merged.Keys.MainKeyFile == nil {
		if err := stopped(ctx); err != nil {
			return Config{}, err
		}
		if cfg.Keys.MainKey, err = credentialSource(src.Credentials); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// missingCertificate reports dev.vite_cert_file and dev.vite_key_file
// missing when dev, as built from the merged layer given, has an https
// Vite dev server: Vite serves https only with both.
func missingCertificate(given devLayer, dev Dev) []error {
	u, ok := dev.ViteURL.Get()
	if !ok || u.Scheme != "https" {
		return nil
	}
	var errs []error
	for _, s := range settings() {
		var v *string
		switch s.key {
		case "dev.vite_cert_file":
			v = given.ViteCertFile
		case "dev.vite_key_file":
			v = given.ViteKeyFile
		default:
			continue
		}
		if v == nil {
			errs = append(errs, fmt.Errorf("%s: %w: the Vite dev server is https, so it needs a certificate and its key; want %s", s.label(), ErrMissing, s.value))
		}
	}
	return errs
}

// stopped returns ctx's error, said to have stopped Load, or nil while
// ctx is live.
func stopped(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("configuration not loaded: %w", err)
	}
	return nil
}

// configFiles is what the configuration files gave.
type configFiles struct {
	// values merges the settings of every file read in full.
	values layer
	// errs holds each problem found reading them.
	errs []error
	// unread names each file or directory that could not be read in
	// full, none of whose settings are used.
	unread []string
}

// readFiles reads pisati.toml, then each pisati.toml.d/*.toml in name
// order, from fsys, checking ctx before each read. Its error is ctx's
// alone: every other problem is in the configFiles.
func readFiles(ctx context.Context, fsys fs.FS) (configFiles, error) {
	var files configFiles
	if fsys == nil {
		return files, nil
	}
	if err := stopped(ctx); err != nil {
		return configFiles{}, err
	}
	files.read(fsys, mainFile)

	if err := stopped(ctx); err != nil {
		return configFiles{}, err
	}
	entries, err := fs.ReadDir(fsys, dropInDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		files.errs = append(files.errs, err)
		files.unread = append(files.unread, dropInDir)
	}
	// fs.ReadDir returns the entries sorted by name.
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		if err := stopped(ctx); err != nil {
			return configFiles{}, err
		}
		files.read(fsys, path.Join(dropInDir, e.Name()))
	}
	return files, nil
}

// read decodes the file name in fsys strictly and merges its settings
// into the values. A file that does not exist gives none. Unknown keys
// alone spoil nothing: go-toml decodes the rest of the file. But go-toml
// stops at any other failure, and may leave the field it was filling set
// to nothing, so such a file gives none of its settings, though a refused
// secret decoded before the failure is still reported.
func (f *configFiles) read(fsys fs.FS, name string) {
	data, err := readBounded(fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		f.errs = append(f.errs, err)
		f.unread = append(f.unread, name)
		return
	}
	var l layer
	err = toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&l)
	f.errs = append(f.errs, decodeErrors(name, err)...)
	if _, unknownOnly := errors.AsType[*toml.StrictMissingError](err); err != nil && !unknownOnly {
		f.errs = append(f.errs, refusals(name, &l)...)
		f.unread = append(f.unread, name)
		return
	}
	overlay(&f.values, &l)
}

// readBounded reads the regular file name from fsys, refusing one larger
// than maxFileSize.
func readBounded(fsys fs.FS, name string) (data []byte, err error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, f.Close())
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w: not a regular file", name, ErrInvalid)
	}
	data, err = io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("%s: %w: larger than 1 MiB", name, ErrInvalid)
	}
	return data, nil
}

// decodeErrors turns what decoding the file name returned into one error
// for each problem, each giving the file, line and column, and the key
// when there is one. They never quote the file, which may hold a secret
// in a key Load refuses.
func decodeErrors(name string, err error) []error {
	if err == nil {
		return nil
	}
	if strict, ok := errors.AsType[*toml.StrictMissingError](err); ok {
		errs := make([]error, 0, len(strict.Errors))
		for i := range strict.Errors {
			row, col := strict.Errors[i].Position()
			errs = append(errs, fmt.Errorf("%s:%d:%d: %s: %w", name, row, col, strings.Join(strict.Errors[i].Key(), "."), ErrUnknown))
		}
		return errs
	}
	const unused = "none of the file's settings are used until it decodes"
	if de, ok := errors.AsType[*toml.DecodeError](err); ok {
		row, col := de.Position()
		where := fmt.Sprintf("%s:%d:%d", name, row, col)
		if key := de.Key(); len(key) > 0 {
			where += ": " + strings.Join(key, ".")
		}
		return []error{fmt.Errorf("%s: %w: %s; %s", where, ErrInvalid, strings.TrimPrefix(de.Error(), "toml: "), unused)}
	}
	return []error{fmt.Errorf("%s: %w: %w; %s", name, ErrInvalid, err, unused)}
}

// refusals reports each refused setting l holds, found in the file name.
func refusals(name string, l *layer) []error {
	var errs []error
	for _, s := range settings() {
		if k, ok := s.kind.(refused); ok && *k.field(l) != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, refusal(s, k)))
		}
	}
	return errs
}

// refusal is the error for the refused setting s, given. It never
// repeats the value, which is the secret itself.
func refusal(s setting, k refused) error {
	return fmt.Errorf("%s: %w: a secret is read only from a file; name the file with %s", s.label(), ErrInvalid, label(k.instead))
}

// build turns the merged layer into a Config, going by the settings
// table: each value given, or each default, goes through its setting's
// apply. It returns the values refused, and apart from them the settings
// missing, which Load reports only when it read every file.
func build(l layer) (c Config, refusedValues, missing []error) {
	for _, s := range settings() {
		switch k := s.kind.(type) {
		case text:
			v, given := deref(*k.field(&l))
			if s.absent == reportMissing && v == "" {
				given = false
			}
			if !given {
				switch s.absent {
				case applyDefault:
					v = s.def
				case reportMissing:
					missing = append(missing, fmt.Errorf("%s: %w: want %s", s.label(), ErrMissing, s.value))
					continue
				case leaveToRules:
					continue
				}
			}
			if err := k.apply(&c, v); err != nil {
				refusedValues = append(refusedValues, fmt.Errorf("%s: %w", s.label(), err))
			}
		case list:
			p := *k.field(&l)
			var items []string
			if p != nil {
				items = *p
			} else {
				switch s.absent {
				case applyDefault:
					items = splitList(s.def)
				case reportMissing:
					missing = append(missing, fmt.Errorf("%s: %w: want %s", s.label(), ErrMissing, s.value))
					continue
				case leaveToRules:
					continue
				}
			}
			for _, err := range k.apply(&c, items) {
				refusedValues = append(refusedValues, fmt.Errorf("%s: %w", s.label(), err))
			}
		case refused:
			if *k.field(&l) != nil {
				refusedValues = append(refusedValues, refusal(s, k))
			}
		}
	}
	return c, refusedValues, missing
}

// deref returns *p and true, or "" and false when p is nil.
func deref(p *string) (string, bool) {
	if p == nil {
		return "", false
	}
	return *p, true
}

// stateDirectory returns systemd's $STATE_DIRECTORY, s, as the state
// directory: one absolute, clean path.
func stateDirectory(s string) (string, error) {
	if strings.Contains(s, ":") {
		return "", fmt.Errorf("%w value %q: it lists more than one directory; give Pisati one", ErrInvalid, s)
	}
	return parsePath(s)
}

// credentialSource returns the credential as the main key's source
// when creds holds it, else Generated.
func credentialSource(creds fs.FS) (SecretSource, error) {
	if creds == nil {
		return Generated{}, nil
	}
	_, err := fs.Stat(creds, mainKeyCredential)
	switch {
	case err == nil:
		return FromCredential{Name: mainKeyCredential}, nil
	case errors.Is(err, fs.ErrNotExist):
		return Generated{}, nil
	default:
		return nil, fmt.Errorf("CREDENTIALS_DIRECTORY: %w", err)
	}
}

// parsePrefix returns s, a CIDR prefix with no host bits set or a bare
// address, as a prefix: a bare address is a /32 or a /128.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%w value %q: want an IP address or a CIDR prefix", ErrInvalid, s)
		}
		if p != p.Masked() {
			return netip.Prefix{}, fmt.Errorf("%w value %q: it sets host bits; the prefix is %s", ErrInvalid, s, p.Masked())
		}
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("%w value %q: want an IP address or a CIDR prefix", ErrInvalid, s)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// levelName spells l as the configuration does: debug, info, warn or
// error.
func levelName(l slog.Level) string {
	return strings.ToLower(l.String())
}

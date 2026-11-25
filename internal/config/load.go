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
	"maps"
	"net/netip"
	"path"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/divbast/pisati/internal/explain"
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
// PISATI_ variable, as UnknownEnv finds them, and every value read that
// its rule refuses. A key is
// known only when spelt exactly as its setting is, in lower case: [SERVER]
// or BAse_url is an unknown key, named as written with its file, whose
// value is not used, and the setting it would set is not also reported
// missing. Decoding a file stops at its first syntax or type error, so a
// file with one is reported once, at that error, and none of its settings
// are used; no setting is then reported missing, as that file may hold
// it. A refused secret it holds before the error is still reported.
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
		errs = append(errs, unknownVariable(name))
	}
	cfg, refusedValues, missing := build(merged, files.otherCase)
	errs = append(errs, refusedValues...)
	if len(files.unread) == 0 {
		errs = append(errs, missing...)
		errs = append(errs, missingCertificate(merged.Dev, cfg.Dev, files.otherCase)...)
	}

	// systemd's $STATE_DIRECTORY replaces the default, but not a value a
	// source gives.
	if merged.Storage.StateDir == nil && stateDirEnv != "" {
		if dir, err := stateDirectory(stateDirEnv); err != nil {
			errs = append(errs, err)
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
// Vite dev server: Vite serves https only with both. A setting a file
// holds in another case, in otherCase, is not reported missing.
func missingCertificate(given devLayer, dev Dev, otherCase map[string]bool) []error {
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
		if v == nil && !otherCase[s.key] {
			errs = append(errs, explain.New(ErrMissing, "%s is not set, but %s is set to an https URL, so the Vite dev server "+
				"needs a certificate and its private key. Set %s to %s", s.label(), label("dev.vite_url"), s.key, s.value))
		}
	}
	return errs
}

// stopped returns ctx's error, said to have stopped Load, or nil while
// ctx is live.
func stopped(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the configuration was not read to its end, because reading it was stopped: %w", err)
	}
	return nil
}

// unknownVariable is the problem of the variable name, which looks like
// one of Pisati's but names no setting.
func unknownVariable(name string) error {
	for _, s := range settings() {
		if s.env() == strings.ToUpper(name) {
			return explain.New(ErrUnknown, "the environment variable %q is set, but Pisati's variables are written in "+
				"capital letters. Write it as %s", name, s.env())
		}
	}
	return explain.New(ErrUnknown, "the environment variable %q is set, but Pisati has no setting of that name. Check its "+
		"spelling, or remove it", name)
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
	// otherCase holds the key of each setting a file holds in another
	// case, as an unknown key, which is reported, and so not reported
	// missing as well.
	otherCase map[string]bool
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
		files.errs = append(files.errs, cannotRead(err, "the directory "+dropInDir+" in the configuration directory"))
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
// secret decoded before the failure is still reported. A file holding a
// key in another case, which go-toml alone would take for its setting, is
// read through readMap instead.
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
	// go-toml matches a key to a field whatever its case, so a file that
	// holds one in another case is read from a map, which keeps keys as
	// written: only those spelt as the settings table spells them count.
	var doc map[string]any
	if toml.Unmarshal(data, &doc) == nil && inOtherCase(doc) {
		f.readMap(name, doc)
		return
	}
	var l layer
	err = toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&l)
	f.errs = append(f.errs, decodeErrors(name, data, err)...)
	if _, unknownOnly := errors.AsType[*toml.StrictMissingError](err); err != nil && !unknownOnly {
		f.errs = append(f.errs, refusals(name, &l)...)
		f.unread = append(f.unread, name)
		return
	}
	overlay(&f.values, &l)
}

// readMap merges the settings of doc, the file name decoded into a map,
// into the values, as fromMap reads them. A value of the wrong type is
// reported as a decode failure is: the file gives none of its settings.
func (f *configFiles) readMap(name string, doc map[string]any) {
	l, unknown, wrongType, otherCase := fromMap(name, doc)
	f.errs = append(f.errs, unknown...)
	if f.otherCase == nil {
		f.otherCase = make(map[string]bool)
	}
	maps.Copy(f.otherCase, otherCase)
	if len(wrongType) > 0 {
		f.errs = append(f.errs, wrongType...)
		f.errs = append(f.errs, refusals(name, &l)...)
		f.unread = append(f.unread, name)
		return
	}
	overlay(&f.values, &l)
}

// readBounded reads the regular file name from fsys, refusing one larger
// than maxFileSize. Its errors name the file, and say why it cannot be
// read; one that does not exist is fs.ErrNotExist alone.
func readBounded(fsys fs.FS, name string) (data []byte, err error) {
	f, err := fsys.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	what := "the configuration file " + strconv.Quote(name)
	if err != nil {
		return nil, cannotRead(err, what)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cannotRead(cerr, what))
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, cannotRead(err, what)
	}
	if !info.Mode().IsRegular() {
		return nil, explain.New(ErrInvalid, "%s is not a regular file, so Pisati cannot read its settings. Replace it "+
			"with a regular file, or remove it", what)
	}
	data, err = io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, cannotRead(err, what)
	}
	if len(data) > maxFileSize {
		return nil, explain.New(ErrInvalid, "%s is larger than %s, which is more than Pisati reads from a "+
			"configuration file. Check that it is the right file", what, explain.Bytes(maxFileSize))
	}
	return data, nil
}

// cannotRead is the problem of what, a file or directory that err says
// cannot be read: it gives the operating system's reason, and, when that
// is a lack of permission, what to do.
func cannotRead(err error, what string) error {
	if errors.Is(err, fs.ErrPermission) {
		return explain.New(err, "Pisati cannot read %s, because the user that runs Pisati does not have permission to. "+
			"Give that user permission to read it", what)
	}
	return explain.New(err, "Pisati cannot read %s: %s. Check that it is a regular file that the user that runs "+
		"Pisati can read", what, pathless(err))
}

// pathless returns the error inside err when err is an *fs.PathError,
// whose path the message around it already names, or err.
func pathless(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// notDecoded is why a file that does not decode is refused: fixed text,
// which never quotes the file. It follows the file, and the line, column
// and key, as decodeError gives them.
const notDecoded = "it is not valid TOML there, or a value of the wrong type, such as a number where a value in " +
	"quotes belongs. Pisati does not show what is there, as it may be a secret, and uses none of the file's " +
	"settings until it is fixed"

// decodeError is the problem of the file name, which does not decode as
// TOML: at line row and column col, when both are above 0, and in key,
// a setting, or a table when it has one part, when it has any.
func decodeError(name string, row, col int, key []string) error {
	where := "the configuration file " + strconv.Quote(name) + " has an error"
	if row > 0 && col > 0 {
		where += fmt.Sprintf(" at line %d, column %d", row, col)
		if len(key) > 0 {
			where += ","
		}
	}
	switch len(key) {
	case 0:
	case 1:
		where += " in the table " + key[0]
	default:
		where += " in the setting " + strings.Join(key, ".")
	}
	return explain.New(ErrInvalid, "%s: %s", where, notDecoded)
}

// decodeErrors turns what decoding data, the file name, returned into one
// error for each problem. An unknown key's gives the file, line and
// column, and the key. Any other gives the file, and the line and column
// when go-toml can tell them, in fixed text: never go-toml's own message,
// which may quote the file, and so a secret in it. It names the key the
// failure is in, as failedKey reads it, only as far as that key is a
// section or a setting: TOML reads a line with no "=" as a key, so a value
// pasted or wrapped onto a line of its own would be named in full.
func decodeErrors(name string, data []byte, err error) []error {
	if err == nil {
		return nil
	}
	if strict, ok := errors.AsType[*toml.StrictMissingError](err); ok {
		errs := make([]error, 0, len(strict.Errors))
		for i := range strict.Errors {
			row, col := strict.Errors[i].Position()
			key := unknownKey(data, offset(data, row, col), strict.Errors[i].Key())
			errs = append(errs, explain.New(ErrUnknown, "the configuration file %q has the key %q, at line %d, column %d, "+
				"but Pisati has no setting of that name. Check its spelling, or remove it", name, strings.Join(key, "."), row, col))
		}
		return errs
	}
	de, ok := errors.AsType[*toml.DecodeError](err)
	if !ok {
		return []error{decodeError(name, 0, 0, nil)}
	}
	row, col := de.Position()
	return []error{decodeError(name, row, col, knownPrefix(failedKey(data, offset(data, row, col))))}
}

// offset returns the offset in data of line row, column col, as go-toml
// counts them from 1, in bytes; the length of data when data has no such
// place.
func offset(data []byte, row, col int) int {
	start := 0
	for range row - 1 {
		n := bytes.IndexByte(data[start:], '\n')
		if n < 0 {
			return len(data)
		}
		start += n + 1
	}
	return min(start+max(col-1, 0), len(data))
}

// refusals reports each refused setting l holds, found in the file name.
func refusals(name string, l *layer) []error {
	var errs []error
	for _, s := range settings() {
		if k, ok := s.kind.(refused); ok && *k.field(l) != nil {
			errs = append(errs, refusal("the configuration file "+strconv.Quote(name)+" sets "+s.key, k))
		}
	}
	return errs
}

// refusal is the error for the refused setting k, given, as given says.
// It never repeats the value, which is the secret itself.
func refusal(given string, k refused) error {
	return explain.New(ErrInvalid, "%s, but Pisati does not take this secret as a setting's value. Put it in a file of its "+
		"own, and set %s to that file's full path. Pisati does not show the value, because it is a secret", given,
		label(k.instead))
}

// build turns the merged layer into a Config, going by the settings
// table: each value given, or each default, goes through its setting's
// apply. It returns the values refused, and apart from them the settings
// missing, which Load reports only when it read every file. A setting a
// file holds in another case, in otherCase, is not reported missing: that
// key is reported unknown.
func build(l layer, otherCase map[string]bool) (c Config, refusedValues, missing []error) {
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
					if !otherCase[s.key] {
						missing = append(missing, notGiven(s))
					}
					continue
				case leaveToRules:
					continue
				}
			}
			if err := k.apply(&c, v); err != nil {
				refusedValues = append(refusedValues, labelled(s, err))
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
					if !otherCase[s.key] {
						missing = append(missing, notGiven(s))
					}
					continue
				case leaveToRules:
					continue
				}
			}
			for _, err := range k.apply(&c, items) {
				refusedValues = append(refusedValues, labelled(s, err))
			}
		case refused:
			if *k.field(&l) != nil {
				refusedValues = append(refusedValues, refusal(s.label()+" is set", k))
			}
		}
	}
	return c, refusedValues, missing
}

// notGiven is the problem of s, which has no default, given by no source.
func notGiven(s setting) error {
	return explain.New(ErrMissing, "%s is not set, or is empty, and it has no default. Set it to %s", s.label(), s.value)
}

// labelled returns err, which s's rule returned for its value, as the
// problem of s: a value refused is said of s, and any other problem
// names s itself.
func labelled(s setting, err error) error {
	if b, ok := errors.AsType[*badValue](err); ok {
		return explain.New(err, "%s", b.of(s.label()))
	}
	return err
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
		return "", explain.New(ErrInvalid, "STATE_DIRECTORY is set to %q, which has \":\" in it. \":\" separates two "+
			"directories there, as systemd writes it for a service with more than one StateDirectory=, and Pisati "+
			"uses one. Set it to one directory, or set %s to the directory Pisati should use", s, label("storage.state_dir"))
	}
	dir, err := parsePath(s)
	if b, ok := errors.AsType[*badValue](err); ok {
		return "", explain.New(err, "%s", b.of("STATE_DIRECTORY"))
	}
	return dir, err
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
	case errors.Is(err, fs.ErrPermission):
		return nil, explain.New(err, "Pisati cannot look for the credential %s in the directory that CREDENTIALS_DIRECTORY "+
			"names, because the user that runs Pisati does not have permission to. Give that user permission to read "+
			"that directory", mainKeyCredential)
	default:
		return nil, explain.New(err, "Pisati cannot look for the credential %s in the directory that CREDENTIALS_DIRECTORY "+
			"names: %s. Check that CREDENTIALS_DIRECTORY names a directory that the user that runs Pisati can read",
			mainKeyCredential, pathless(err))
	}
}

// parsePrefix returns s, a CIDR prefix with no host bits set or a bare
// address, as a prefix: a bare address is a /32 or a /128.
func parsePrefix(s string) (netip.Prefix, error) {
	const (
		notOne = "is not an IP address or a range of IP addresses in CIDR form"
		write  = "Write each item as an IP address, such as 192.0.2.1, or as a range, such as 192.0.2.0/24"
	)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, &badValue{value: s, item: true, why: notOne, fix: write}
		}
		if p != p.Masked() {
			return netip.Prefix{}, &badValue{
				value: s, item: true,
				why: "is a range written from an address inside it, not from its first address",
				fix: "Write it as " + p.Masked().String(),
			}
		}
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	switch {
	case err != nil:
		return netip.Prefix{}, &badValue{value: s, item: true, why: notOne, fix: write}
	case a.Zone() != "":
		return netip.Prefix{}, &badValue{
			value: s, item: true,
			why: "has a zone, such as %eth0, after its address, which Pisati does not take", fix: "Remove the zone",
		}
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// levelName spells l as the configuration does: debug, info, warn or
// error.
func levelName(l slog.Level) string {
	return strings.ToLower(l.String())
}

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"path"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	// mainFile and dropInDir are read from Sources.Files.
	mainFile  = "pisati.toml"
	dropInDir = "pisati.toml.d"

	// maxFileSize bounds each configuration file, far above any real one.
	maxFileSize = 1 << 20

	// masterKeyCredential is the credential the master key is read from.
	masterKeyCredential = "pisati-master-key"

	defaultListen         = ":8080"
	defaultInternalListen = ":8081"
	defaultStateDir       = "/var/lib/pisati"
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

// Load returns the configuration src gives, or an error that joins every
// problem it found, each naming the setting, file or variable at fault.
//
// Each source overrides the ones before it: the built-in defaults, then
// pisati.toml, then each pisati.toml.d/*.toml in name order, then the
// PISATI_<SECTION>_<KEY> variables. Tables merge, and a list replaces the
// one before it. A missing pisati.toml or pisati.toml.d is no error.
// stateDirEnv is $STATE_DIRECTORY, or empty when it is not set: when set,
// it replaces the default state directory.
//
// A secret is never given in place: keys.master_key in any source is an
// error. The master key comes from the file keys.master_key_file names,
// else the credential pisati-master-key when Credentials holds it, else
// it is Generated.
//
// On an error, the Config is the zero Config.
func Load(src Sources, stateDirEnv string) (Config, error) {
	merged, errs := readFiles(src.Files)
	env := fromEnv(src.Env)
	overlay(&merged, &env)
	for _, name := range UnknownEnv(src.EnvNames) {
		errs = append(errs, fmt.Errorf("%s: %w", name, ErrUnknown))
	}
	cfg, buildErrs := build(merged, src.Credentials, stateDirEnv)
	errs = append(errs, buildErrs...)
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// readFiles merges pisati.toml and pisati.toml.d/*.toml from fsys, in
// that order and the drop-ins in name order.
func readFiles(fsys fs.FS) (layer, []error) {
	var merged layer
	if fsys == nil {
		return merged, nil
	}
	names := []string{mainFile}
	entries, err := fs.ReadDir(fsys, dropInDir)
	var errs []error
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		errs = append(errs, err)
	}
	// fs.ReadDir returns the entries sorted by name.
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, path.Join(dropInDir, e.Name()))
		}
	}
	for _, name := range names {
		l, fileErrs := readFile(fsys, name)
		errs = append(errs, fileErrs...)
		overlay(&merged, &l)
	}
	return merged, errs
}

// readFile decodes the file name in fsys strictly into a layer. A file
// that does not exist gives an empty layer, and so does one that fails
// to decode: go-toml stops at the failure, and may leave the field it
// was filling set to nothing. Unknown keys alone fail nothing: go-toml
// decodes the rest of the file, and the layer keeps it.
func readFile(fsys fs.FS, name string) (layer, []error) {
	data, err := readBounded(fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		return layer{}, nil
	}
	if err != nil {
		return layer{}, []error{err}
	}
	var l layer
	err = toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&l)
	if _, unknownOnly := errors.AsType[*toml.StrictMissingError](err); err != nil && !unknownOnly {
		return layer{}, decodeErrors(name, err)
	}
	return l, decodeErrors(name, err)
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
	if de, ok := errors.AsType[*toml.DecodeError](err); ok {
		row, col := de.Position()
		where := fmt.Sprintf("%s:%d:%d", name, row, col)
		if key := de.Key(); len(key) > 0 {
			where += ": " + strings.Join(key, ".")
		}
		return []error{fmt.Errorf("%s: %w: %s", where, ErrInvalid, strings.TrimPrefix(de.Error(), "toml: "))}
	}
	return []error{fmt.Errorf("%s: %w: %w", name, ErrInvalid, err)}
}

// build turns the merged layer into a Config, putting each value through
// its type's constructor, with the defaults for what no source set.
func build(l layer, creds fs.FS, stateDirEnv string) (Config, []error) {
	var c Config
	var errs []error
	check := func(key string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label(key), err))
		}
	}
	var err error

	baseOK := false
	if l.Server.BaseURL == nil || *l.Server.BaseURL == "" {
		errs = append(errs, fmt.Errorf("%s: %w: give the https URL the site is served at", label("server.base_url"), ErrMissing))
	} else {
		c.Server.BaseURL, err = ParseBaseURL(*l.Server.BaseURL)
		check("server.base_url", err)
		baseOK = err == nil
	}
	c.Server.Listen, err = ParseListenAddr(or(l.Server.Listen, defaultListen))
	check("server.listen", err)
	c.Server.InternalListen, err = ParseListenAddr(or(l.Server.InternalListen, defaultInternalListen))
	check("server.internal_listen", err)
	var proxies []string
	if l.Server.TrustedProxies != nil {
		proxies = *l.Server.TrustedProxies
	}
	c.Server.TrustedProxies = make([]netip.Prefix, 0, len(proxies))
	for _, p := range proxies {
		if prefix, err := parsePrefix(p); err != nil {
			check("server.trusted_proxies", err)
		} else {
			c.Server.TrustedProxies = append(c.Server.TrustedProxies, prefix)
		}
	}
	if v := l.Server.HSTS; v != nil {
		c.Server.HSTS, err = parseChoice(*v, HSTSScope.String, HSTSHostOnly, HSTSIncludeSubdomains, HSTSPreload)
		check("server.hsts", err)
	}

	switch {
	case l.Storage.StateDir != nil:
		c.Storage.StateDir, err = parsePath(*l.Storage.StateDir)
		check("storage.state_dir", err)
	case strings.Contains(stateDirEnv, ":"):
		errs = append(errs, fmt.Errorf("STATE_DIRECTORY: %w value %q: it lists more than one directory; give Pisati one", ErrInvalid, stateDirEnv))
	case stateDirEnv != "":
		c.Storage.StateDir, err = parsePath(stateDirEnv)
		if err != nil {
			errs = append(errs, fmt.Errorf("STATE_DIRECTORY: %w", err))
		}
	default:
		c.Storage.StateDir = defaultStateDir
	}

	if v := l.Log.Level; v != nil {
		c.Log.Level, err = parseChoice(*v, levelName, slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError)
		check("log.level", err)
	}
	if v := l.Log.Format; v != nil {
		c.Log.Format, err = parseChoice(*v, LogFormat.String, LogJSON, LogText)
		check("log.format", err)
	}

	c.Telemetry.OTLPEndpoint, err = ParseOptionalURL(or(l.Telemetry.OTLPEndpoint, ""))
	check("telemetry.otlp_endpoint", err)
	if v := l.Telemetry.Pprof; v != nil {
		c.Telemetry.Pprof, err = parseChoice(*v, Switch.String, Off, On)
		check("telemetry.pprof", err)
	}

	// The value itself is never repeated: it would put the secret in
	// the error.
	if l.Keys.MasterKey != nil {
		errs = append(errs, fmt.Errorf("%s: %w: a secret is read only from a file; name the file with %s", label("keys.master_key"), ErrInvalid, label("keys.master_key_file")))
	}
	c.Keys.MasterKey, err = masterKeySource(l.Keys.MasterKeyFile, creds)
	if err != nil {
		errs = append(errs, err)
	}

	c.Dev.ViteURL, err = ParseOptionalURL(or(l.Dev.ViteURL, ""))
	check("dev.vite_url", err)
	if u, ok := c.Dev.ViteURL.Get(); ok && baseOK && (!c.Server.BaseURL.local || !loopback(u.Hostname())) {
		errs = append(errs, fmt.Errorf("%s: %w value %q: the Vite dev server is for development alone, so it and the base URL must both be on localhost, 127.0.0.1 or [::1]", label("dev.vite_url"), ErrInvalid, u.String()))
	}
	return c, errs
}

// masterKeySource returns where the master key comes from: the file
// keys.master_key_file names, else the credential when creds holds it,
// else Generated.
func masterKeySource(file *string, creds fs.FS) (SecretSource, error) {
	if file != nil {
		p, err := parsePath(*file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label("keys.master_key_file"), err)
		}
		return FromFile{Path: p}, nil
	}
	if creds == nil {
		return Generated{}, nil
	}
	_, err := fs.Stat(creds, masterKeyCredential)
	switch {
	case err == nil:
		return FromCredential{Name: masterKeyCredential}, nil
	case errors.Is(err, fs.ErrNotExist):
		return Generated{}, nil
	default:
		return nil, fmt.Errorf("CREDENTIALS_DIRECTORY: %w", err)
	}
}

// parsePath returns s when it is an absolute, clean path.
func parsePath(s string) (string, error) {
	if !filepath.IsAbs(s) || filepath.Clean(s) != s {
		return "", fmt.Errorf("%w value %q: want an absolute, clean path", ErrInvalid, s)
	}
	return s, nil
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

// or returns *p, or def when p is nil.
func or(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

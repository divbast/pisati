// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"

	"github.com/divbast/pisati/internal/config"
)

const baseURL = "https://blog.example.org"

// everyKey sets every setting a file can hold, each away from its default.
const everyKey = `
[server]
base_url = "https://file.example.org"
listen = "127.0.0.1:7000"
internal_listen = "127.0.0.1:7001"
trusted_proxies = ["10.0.0.0/8"]
hsts = "include-subdomains"

[storage]
state_dir = "/srv/file"

[log]
level = "warn"
format = "text"

[telemetry]
otlp_endpoint = "http://collector.file:4318"
pprof = "on"

[keys]
main_key_file = "/run/file/key"

[dev]
vite_url = ""
vite_cert_file = "/srv/file/vite.pem"
vite_key_file = "/srv/file/vite-key.pem"
`

// env is a fake environment: the variables a test sets, and no others.
type env map[string]string

// sources gives Load e as its environment, and nothing else.
func (e env) sources() config.Sources {
	return config.Sources{
		Env: func(name string) (string, bool) {
			v, ok := e[name]
			return v, ok
		},
		EnvNames: slices.Sorted(maps.Keys(e)),
	}
}

// setup is what one Load call is given.
type setup struct {
	files fstest.MapFS
	env   env
	creds fs.FS
	state string
}

func (s setup) load(ctx context.Context) (config.Config, error) {
	src := s.env.sources()
	if s.files != nil {
		src.Files = s.files
	}
	src.Credentials = s.creds
	return config.Load(ctx, src, s.state)
}

func tomlFile(s string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(s)}
}

// describe spells out every setting in c, keyed by its TOML key.
func describe(c config.Config) map[string]string {
	return map[string]string{
		"server.base_url":         c.Server.BaseURL.String(),
		"server.listen":           c.Server.Listen.String(),
		"server.internal_listen":  c.Server.InternalListen.String(),
		"server.trusted_proxies":  fmt.Sprint(c.Server.TrustedProxies),
		"server.hsts":             c.Server.HSTS.String(),
		"storage.state_dir":       c.Storage.StateDir,
		"log.level":               c.Log.Level.String(),
		"log.format":              c.Log.Format.String(),
		"telemetry.otlp_endpoint": optional(c.Telemetry.OTLPEndpoint),
		"telemetry.pprof":         c.Telemetry.Pprof.String(),
		"keys.main_key":           source(c.Keys.MainKey),
		"dev.vite_url":            optional(c.Dev.ViteURL),
		"dev.vite_cert_file":      c.Dev.ViteCertFile,
		"dev.vite_key_file":       c.Dev.ViteKeyFile,
	}
}

func optional(o config.OptionalURL) string {
	u, ok := o.Get()
	if !ok {
		return "unset"
	}
	return u.String()
}

func source(s config.SecretSource) string {
	switch s := s.(type) {
	case config.FromFile:
		return "file " + s.Path
	case config.FromCredential:
		return "credential " + s.Name
	case config.Generated:
		return "generated"
	case nil:
		return "nil"
	}
	return fmt.Sprintf("unknown %T", s)
}

// sameConfig fails t for every setting of got that is not as want says.
func sameConfig(t *testing.T, got config.Config, want map[string]string) {
	t.Helper()
	have := describe(got)
	for _, key := range slices.Sorted(maps.Keys(have)) {
		if have[key] != want[key] {
			t.Errorf("%s = %q, want %q", key, have[key], want[key])
		}
	}
}

// refused fails t unless err is an error whose text names every one of
// names, and which wraps want.
func refused(t *testing.T, err, want error, names ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Load succeeded, want an error naming %q", names)
	}
	if !errors.Is(err, want) {
		t.Errorf("error %q does not wrap %q", err, want)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %q", err, name)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := setup{env: env{"PISATI_SERVER_BASE_URL": baseURL}}.load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sameConfig(t, cfg, map[string]string{
		"server.base_url":         baseURL,
		"server.listen":           ":8080",
		"server.internal_listen":  ":8081",
		"server.trusted_proxies":  "[]",
		"server.hsts":             "host-only",
		"storage.state_dir":       "/var/lib/pisati",
		"log.level":               "INFO",
		"log.format":              "json",
		"telemetry.otlp_endpoint": "unset",
		"telemetry.pprof":         "off",
		"keys.main_key":           "generated",
		"dev.vite_url":            "unset",
		"dev.vite_cert_file":      "",
		"dev.vite_key_file":       "",
	})
	if !cfg.Server.BaseURL.IsHTTPS() {
		t.Errorf("%s: IsHTTPS = false", cfg.Server.BaseURL)
	}
	if cfg.Server.Listen.Port() != 8080 || cfg.Server.InternalListen.Port() != 8081 {
		t.Errorf("ports %d and %d, want 8080 and 8081", cfg.Server.Listen.Port(), cfg.Server.InternalListen.Port())
	}

	// Each closed set's default is its zero value.
	var zero config.Config
	if zero.Server.HSTS != config.HSTSHostOnly || zero.Log.Format != config.LogJSON || zero.Telemetry.Pprof != config.Off {
		t.Errorf("zero values %v, %v, %v, want host-only, json, off", zero.Server.HSTS, zero.Log.Format, zero.Telemetry.Pprof)
	}
	if cfg.Server.HSTS != config.HSTSHostOnly || cfg.Log.Format != config.LogJSON || cfg.Telemetry.Pprof != config.Off {
		t.Errorf("defaults %v, %v, %v, want host-only, json, off", cfg.Server.HSTS, cfg.Log.Format, cfg.Telemetry.Pprof)
	}
	if _, ok := zero.Telemetry.OTLPEndpoint.Get(); ok {
		t.Error("the zero OptionalURL is set")
	}
}

func TestLoadRequiresBaseURL(t *testing.T) {
	t.Parallel()

	for name, src := range map[string]config.Sources{
		"nothing given": {},
		"empty":         env{"PISATI_SERVER_BASE_URL": ""}.sources(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(t.Context(), src, "")
			refused(t, err, config.ErrMissing, "server.base_url", "PISATI_SERVER_BASE_URL", "https")
		})
	}
}

func TestDropInsMergeInNameOrder(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"pisati.toml": tomlFile(`
[server]
base_url = "https://blog.example.org"
listen = ":9000"
trusted_proxies = ["10.0.0.0/8", "192.168.0.0/16"]
hsts = "include-subdomains"

[log]
level = "debug"
`),
		// Written out of order: Load sorts them by name.
		"pisati.toml.d/20-late.toml":    tomlFile("[server]\nlisten = \":9002\"\n\n[log]\nformat = \"text\"\n"),
		"pisati.toml.d/10-early.toml":   tomlFile("[server]\nlisten = \":9001\"\ntrusted_proxies = [\"172.16.0.0/12\"]\n"),
		"pisati.toml.d/30-old.toml.bak": tomlFile("not TOML ["),
		"pisati.toml.d/README":          tomlFile("not TOML either ["),
	}
	cfg, err := setup{files: files}.load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sameConfig(t, cfg, map[string]string{
		"server.base_url":         baseURL,
		"server.listen":           ":9002",           // the last drop-in wins
		"server.internal_listen":  ":8081",           // nothing set it
		"server.trusted_proxies":  "[172.16.0.0/12]", // replaced, not appended
		"server.hsts":             "include-subdomains",
		"storage.state_dir":       "/var/lib/pisati",
		"log.level":               "DEBUG", // [log] merged with the drop-in's
		"log.format":              "text",
		"telemetry.otlp_endpoint": "unset",
		"telemetry.pprof":         "off",
		"keys.main_key":           "generated",
		"dev.vite_url":            "unset",
	})

	// Name order is byte order, so 9 sorts after 10, and drop-ins need no
	// pisati.toml.
	cfg, err = setup{files: fstest.MapFS{
		"pisati.toml.d/9-last.toml":   tomlFile("[server]\nbase_url = \"https://nine.example.org\"\n"),
		"pisati.toml.d/10-first.toml": tomlFile("[server]\nbase_url = \"https://ten.example.org\"\n"),
	}}.load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Server.BaseURL.String(); got != "https://nine.example.org" {
		t.Errorf("base URL %q, want the one from 9-last.toml", got)
	}
}

func TestEnvWinsOverFiles(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{"pisati.toml": tomlFile(everyKey)}
	cfg, err := setup{files: files}.load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sameConfig(t, cfg, map[string]string{
		"server.base_url":         "https://file.example.org",
		"server.listen":           "127.0.0.1:7000",
		"server.internal_listen":  "127.0.0.1:7001",
		"server.trusted_proxies":  "[10.0.0.0/8]",
		"server.hsts":             "include-subdomains",
		"storage.state_dir":       "/srv/file",
		"log.level":               "WARN",
		"log.format":              "text",
		"telemetry.otlp_endpoint": "http://collector.file:4318",
		"telemetry.pprof":         "on",
		"keys.main_key":           "file /run/file/key",
		"dev.vite_url":            "unset",
		"dev.vite_cert_file":      "/srv/file/vite.pem",
		"dev.vite_key_file":       "/srv/file/vite-key.pem",
	})

	cfg, err = setup{files: files, env: env{
		"PISATI_SERVER_BASE_URL":         "http://localhost:8080",
		"PISATI_SERVER_LISTEN":           "[::1]:9000",
		"PISATI_SERVER_INTERNAL_LISTEN":  "[::1]:9001",
		"PISATI_SERVER_TRUSTED_PROXIES":  "192.0.2.0/24, 2001:db8::/32",
		"PISATI_SERVER_HSTS":             "preload",
		"PISATI_STORAGE_STATE_DIR":       "/srv/env",
		"PISATI_LOG_LEVEL":               "error",
		"PISATI_LOG_FORMAT":              "json",
		"PISATI_TELEMETRY_OTLP_ENDPOINT": "https://collector.env/v1/traces",
		"PISATI_TELEMETRY_PPROF":         "off",
		"PISATI_KEYS_MAIN_KEY_FILE":      "/run/env/key",
		"PISATI_DEV_VITE_URL":            "http://localhost:5173",
		"PISATI_DEV_VITE_CERT_FILE":      "/srv/env/vite.pem",
		"PISATI_DEV_VITE_KEY_FILE":       "/srv/env/vite-key.pem",
	}}.load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sameConfig(t, cfg, map[string]string{
		"server.base_url":         "http://localhost:8080",
		"server.listen":           "[::1]:9000",
		"server.internal_listen":  "[::1]:9001",
		"server.trusted_proxies":  "[192.0.2.0/24 2001:db8::/32]",
		"server.hsts":             "preload",
		"storage.state_dir":       "/srv/env",
		"log.level":               "ERROR",
		"log.format":              "json",
		"telemetry.otlp_endpoint": "https://collector.env/v1/traces",
		"telemetry.pprof":         "off",
		"keys.main_key":           "file /run/env/key",
		"dev.vite_url":            "http://localhost:5173",
		"dev.vite_cert_file":      "/srv/env/vite.pem",
		"dev.vite_key_file":       "/srv/env/vite-key.pem",
	})
}

func TestFileVariable(t *testing.T) {
	t.Parallel()

	const secret = "correct-horse-battery-staple"

	cfg, err := setup{env: env{
		"PISATI_SERVER_BASE_URL":    baseURL,
		"PISATI_KEYS_MAIN_KEY_FILE": "/run/secrets/pisati-main-key",
	}}.load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := source(cfg.Keys.MainKey); got != "file /run/secrets/pisati-main-key" {
		t.Errorf("main key from %s, want the file", got)
	}

	tests := map[string]setup{
		"both forms": {env: env{
			"PISATI_SERVER_BASE_URL":    baseURL,
			"PISATI_KEYS_MAIN_KEY":      secret,
			"PISATI_KEYS_MAIN_KEY_FILE": "/run/secrets/pisati-main-key",
		}},
		"the value alone": {env: env{
			"PISATI_SERVER_BASE_URL": baseURL,
			"PISATI_KEYS_MAIN_KEY":   secret,
		}},
		"an empty value": {env: env{
			"PISATI_SERVER_BASE_URL": baseURL,
			"PISATI_KEYS_MAIN_KEY":   "",
		}},
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := s.load(t.Context())
			refused(t, err, config.ErrInvalid, "(PISATI_KEYS_MAIN_KEY)", "(PISATI_KEYS_MAIN_KEY_FILE)")
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q holds the secret", err)
			}
		})
	}

	t.Run("in a file", func(t *testing.T) {
		t.Parallel()

		_, err := setup{
			files: fstest.MapFS{"pisati.toml": tomlFile("[keys]\nmain_key = \"" + secret + "\"\n")},
			env:   env{"PISATI_SERVER_BASE_URL": baseURL},
		}.load(t.Context())
		refused(t, err, config.ErrInvalid, "keys.main_key ", "keys.main_key_file", "PISATI_KEYS_MAIN_KEY_FILE")
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q holds the secret", err)
		}
	})

	t.Run("a relative path", func(t *testing.T) {
		t.Parallel()

		_, err := setup{env: env{
			"PISATI_SERVER_BASE_URL":    baseURL,
			"PISATI_KEYS_MAIN_KEY_FILE": "secrets/key",
		}}.load(t.Context())
		refused(t, err, config.ErrInvalid, "PISATI_KEYS_MAIN_KEY_FILE", "absolute")
	})

	// The key itself, given by mistake where its file's path belongs, is
	// never repeated. Built here, so the source holds no key-like text.
	key := strings.Repeat("q7Zx+9kP", 4) + "=="
	for name, s := range map[string]setup{
		"the key in the variable": {env: env{
			"PISATI_SERVER_BASE_URL":    baseURL,
			"PISATI_KEYS_MAIN_KEY_FILE": key,
		}},
		"the key in a file": {
			files: fstest.MapFS{"pisati.toml": tomlFile("[keys]\nmain_key_file = \"" + key + "\"\n")},
			env:   env{"PISATI_SERVER_BASE_URL": baseURL},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := s.load(t.Context())
			refused(t, err, config.ErrInvalid, "keys.main_key_file (PISATI_KEYS_MAIN_KEY_FILE)", "absolute")
			if strings.Contains(err.Error(), key[:8]) {
				t.Errorf("error %q repeats the value", err)
			}
		})
	}
}

func TestUnknownTOMLKeyAndEnvNamed(t *testing.T) {
	t.Parallel()

	_, err := setup{
		files: fstest.MapFS{
			"pisati.toml":                 tomlFile("[server]\nbase_url = \"https://blog.example.org\"\nbsae_url = \"x\"\n\n[nope]\nkey = 1\n"),
			"pisati.toml.d/10-extra.toml": tomlFile("toplevel = true\n"),
		},
		env: env{
			"PISATI_SERVER_BSAE_URL": "x",
			"PISATI_SERVER_LISTEN":   ":9000",
			"HOME":                   "/home/someone",
		},
	}.load(t.Context())
	refused(t, err, config.ErrUnknown,
		"pisati.toml:3:1: server.bsae_url",
		"nope",
		"pisati.toml.d/10-extra.toml:1:1: toplevel",
		"PISATI_SERVER_BSAE_URL",
	)
	for _, known := range []string{"HOME", "PISATI_SERVER_LISTEN"} {
		if strings.Contains(err.Error(), known) {
			t.Errorf("error %q names %s, which is not unknown", err, known)
		}
	}

	t.Run("values after an unknown key", func(t *testing.T) {
		t.Parallel()

		_, err := setup{
			files: fstest.MapFS{"pisati.toml": tomlFile("[server]\nbsae_url = 1\nbase_url = \"https://blog.example.org\"\nlisten = \"nope\"\n")},
		}.load(t.Context())
		refused(t, err, config.ErrUnknown, "server.bsae_url")
		refused(t, err, config.ErrInvalid, "server.listen (PISATI_SERVER_LISTEN)", `"nope"`)
		if errors.Is(err, config.ErrMissing) {
			t.Errorf("error %q reports the base URL missing, though the file gives it after the unknown key", err)
		}
	})

	// A file that fails to decode gives no values, and says so. It may
	// hold the base URL, so the base URL is not reported missing.
	brokenFile := func(t *testing.T, err error, at string) {
		t.Helper()
		refused(t, err, config.ErrInvalid, at, "none of the file's settings are used")
		if errors.Is(err, config.ErrMissing) {
			t.Errorf("error %q reports a setting missing, though the file that failed may hold it", err)
		}
	}

	t.Run("a value of the wrong type", func(t *testing.T) {
		t.Parallel()

		_, err := setup{
			files: fstest.MapFS{"pisati.toml": tomlFile("[server]\nbase_url = \"https://blog.example.org\"\nlisten = 8080\n")},
		}.load(t.Context())
		brokenFile(t, err, "pisati.toml:3:10: server.listen")
		if strings.Contains(err.Error(), "(PISATI_SERVER_LISTEN)") {
			t.Errorf("error %q reports server.listen twice", err)
		}
	})

	t.Run("not TOML", func(t *testing.T) {
		t.Parallel()

		_, err := setup{
			files: fstest.MapFS{"pisati.toml.d/10-broken.toml": tomlFile("[server]\nbase_url = \"https://blog.example.org\"\n[log\n")},
		}.load(t.Context())
		brokenFile(t, err, "pisati.toml.d/10-broken.toml:3:")
	})

	t.Run("a secret in a file that fails to decode", func(t *testing.T) {
		t.Parallel()

		const secret = "correct-horse-battery-staple"
		for name, file := range map[string]string{
			"before the failure": "[keys]\nmain_key = \"" + secret + "\"\n[server\n",
			"the failure itself": "[keys]\nmain_key = [\"" + secret + "\"]\n",
		} {
			_, err := setup{
				files: fstest.MapFS{"pisati.toml": tomlFile(file)},
				env:   env{"PISATI_SERVER_BASE_URL": baseURL},
			}.load(t.Context())
			refused(t, err, config.ErrInvalid, "pisati.toml: keys.main_key (PISATI_KEYS_MAIN_KEY)", "keys.main_key_file (PISATI_KEYS_MAIN_KEY_FILE)")
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error %q holds the secret", name, err)
			}
		}
	})
}

// knownEnv is every variable Pisati reads, in the table's order.
var knownEnv = []string{
	"PISATI_SERVER_BASE_URL", "PISATI_SERVER_LISTEN", "PISATI_SERVER_INTERNAL_LISTEN",
	"PISATI_SERVER_TRUSTED_PROXIES", "PISATI_SERVER_HSTS", "PISATI_STORAGE_STATE_DIR",
	"PISATI_LOG_LEVEL", "PISATI_LOG_FORMAT", "PISATI_TELEMETRY_OTLP_ENDPOINT",
	"PISATI_TELEMETRY_PPROF", "PISATI_KEYS_MAIN_KEY", "PISATI_KEYS_MAIN_KEY_FILE",
	"PISATI_DEV_VITE_URL", "PISATI_DEV_VITE_CERT_FILE", "PISATI_DEV_VITE_KEY_FILE",
}

func TestUnknownEnv(t *testing.T) {
	t.Parallel()

	names := []string{
		"HOME", "PISATI", "PISATI_NOPE", "PISATI_SERVER_BASE_URL",
		"pisati_server_base_url", "PISATI_NOPE", "PISATI_",
		"PISATI_KEYS_MAIN_KEY", "PISATI_KEYS_MAIN_KEY_FILE",
	}
	before := slices.Clone(names)
	got := config.UnknownEnv(names)
	if want := []string{"PISATI_", "PISATI_NOPE", "pisati_server_base_url"}; !slices.Equal(got, want) {
		t.Errorf("UnknownEnv = %q, want %q", got, want)
	}
	if !slices.Equal(names, before) {
		t.Errorf("UnknownEnv changed its argument to %q", names)
	}

	if got := config.UnknownEnv(knownEnv); len(got) != 0 {
		t.Errorf("UnknownEnv(every known name) = %q, want none", got)
	}
}

func TestSettingsTable(t *testing.T) {
	t.Parallel()

	table := config.Table()
	keys := make([]string, 0, len(table))
	envs := make([]string, 0, len(table))
	for _, r := range table {
		keys = append(keys, r.Key)
		envs = append(envs, r.Env)
		if want := "PISATI_" + strings.ToUpper(strings.ReplaceAll(r.Key, ".", "_")); r.Env != want {
			t.Errorf("%s: variable %s, want %s", r.Key, r.Env, want)
		}
		if r.Value == "" {
			t.Errorf("%s: no spelling of its value", r.Key)
		}
	}

	// A field with a tag but no row would be decoded and then ignored,
	// and a row reaching the wrong field would set another setting.
	if got := config.LayerKeys(); !slices.Equal(got, keys) {
		t.Errorf("the TOML fields are %q, but the table has %q", got, keys)
	}
	if got := config.SlotKeys(); !slices.Equal(got, keys) {
		t.Errorf("the table's rows reach the fields %q, want %q", got, keys)
	}
	if !slices.Equal(envs, knownEnv) {
		t.Errorf("the table's variables are %q, want %q", envs, knownEnv)
	}
	if got := config.UnknownEnv(envs); len(got) != 0 {
		t.Errorf("UnknownEnv does not know %q", got)
	}

	// What the table gives as each default is what Load applies when no
	// source gives the setting.
	defaults, err := setup{env: env{"PISATI_SERVER_BASE_URL": baseURL}}.load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, r := range table {
		switch r.Absent {
		case "default":
			cfg, err := setup{env: env{"PISATI_SERVER_BASE_URL": baseURL, r.Env: r.Default}}.load(t.Context())
			if err != nil || !reflect.DeepEqual(cfg, defaults) {
				t.Errorf("%s=%q, its default, gives %+v, %v; want what giving nothing gives, %+v", r.Env, r.Default, cfg, err, defaults)
			}
		case "missing":
			_, err := setup{}.load(t.Context())
			refused(t, err, config.ErrMissing, r.Key, r.Env)
		case "rules", "refused":
		default:
			t.Errorf("%s: absent %q", r.Key, r.Absent)
		}
	}
}

// errorParts returns the errors err joins, or err alone.
func errorParts(err error) []error {
	if joined, ok := errors.AsType[interface {
		error
		Unwrap() []error
	}](err); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

// TestKeysAreLowerCase gives files keys spelt otherwise than the settings
// table spells them, which go-toml alone would match whatever their case:
// each is an unknown key, named as written with its file. Its value is not
// used, and the setting it would set is not reported missing as well.
func TestKeysAreLowerCase(t *testing.T) {
	t.Parallel()

	const base = "\"https://file.example.org\""
	tests := []struct {
		name, file, text string
		want             []string // the errors, but for ones of a value refused
		refused          string   // the error of a value refused, if there is one
	}{
		{name: "a table", text: "[SERVER]\nbase_url = " + base + "\n", want: []string{"pisati.toml: SERVER"}},
		{name: "a key", text: "[server]\nBAse_url = " + base + "\n", want: []string{"pisati.toml: server.BAse_url"}},
		{
			name: "the same key twice, in two cases",
			text: "[server]\nbase_url = " + base + "\nBASE_URL = \"http://plain.example.org\"\n",
			want: []string{"pisati.toml: server.BASE_URL"},
		},
		{name: "a dotted key", text: "SERVER.base_url = " + base + "\n", want: []string{"pisati.toml: SERVER"}},
		{
			name: "a key in an inline table",
			text: "server = { listen = \":9000\", BASE_URL = " + base + " }\n",
			want: []string{"pisati.toml: server.BASE_URL"},
		},
		{name: "a quoted key", text: "[server]\n\"Base_Url\" = " + base + "\n", want: []string{"pisati.toml: server.Base_Url"}},
		// The Kelvin sign, which strings.ToLower makes a k, as go-toml
		// lowers a key to match it.
		{
			name: "a quoted table go-toml lowers",
			text: "[\"\u212aeys\"]\nmain_key_file = \"/run/key\"\n[server]\nbase_url = " + base + "\n",
			want: []string{"pisati.toml: \u212aeys"},
		},
		{name: "an array of tables", text: "[[SERVER]]\nbase_url = " + base + "\n", want: []string{"pisati.toml: SERVER"}},
		{
			name: "an inline array of tables",
			text: "SERVER = [{ listen = \":9000\" }, { base_url = " + base + " }]\n",
			want: []string{"pisati.toml: SERVER"},
		},
		{
			name: "a table in a drop-in", file: "pisati.toml.d/10-log.toml",
			text: "[Log]\nlevel = \"debug\"\n[server]\nbase_url = " + base + "\n",
			want: []string{"pisati.toml.d/10-log.toml: Log"},
		},
		{
			name: "an https dev server's certificate in another case",
			text: "[server]\nbase_url = \"http://localhost:8080\"\n[dev]\nvite_url = \"https://localhost:5173\"\n" +
				"Vite_Cert_File = \"/certs/localhost.pem\"\nvite_key_file = \"/certs/localhost-key.pem\"\n",
			want: []string{"pisati.toml: dev.Vite_Cert_File"},
		},
		{
			name: "a key in another case, of another type",
			text: "[server]\nBAse_url = 1\nbase_url = " + base + "\n",
			want: []string{"pisati.toml: server.BAse_url"},
		},
		{
			name: "a misspelt key beside one in another case",
			text: "[server]\nBASE_URL = " + base + "\nbsae_url = " + base + "\n[nope]\n",
			want: []string{"pisati.toml: nope", "pisati.toml: server.BASE_URL", "pisati.toml: server.bsae_url"},
		},
		{
			name:    "a value of the wrong type beside a key in another case",
			text:    "[server]\nBASE_URL = " + base + "\nlisten = 8080\n",
			want:    []string{"pisati.toml: server.BASE_URL"},
			refused: "pisati.toml: server.listen: invalid: it is not TOML, or not a value the setting takes (what the file holds is not shown); none of the file's settings are used until it decodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name := cmp.Or(tt.file, "pisati.toml")
			cfg, err := setup{files: fstest.MapFS{name: tomlFile(tt.text)}}.load(t.Context())
			if err == nil {
				t.Fatalf("Load succeeded, want %q", tt.want)
			}
			// Those keys alone: what was under them is not read as
			// anything else, and nothing is reported missing.
			var unknown, other []string
			for _, part := range errorParts(err) {
				if errors.Is(part, config.ErrUnknown) {
					unknown = append(unknown, part.Error())
				} else {
					other = append(other, part.Error())
				}
			}
			want := make([]string, 0, len(tt.want))
			for _, w := range tt.want {
				want = append(want, w+": unknown setting")
			}
			if !slices.Equal(unknown, want) {
				t.Errorf("unknown keys %q, want %q", unknown, want)
			}
			if wantOther := slices.DeleteFunc([]string{tt.refused}, func(s string) bool { return s == "" }); !slices.Equal(other, wantOther) {
				t.Errorf("other errors %q, want %q", other, wantOther)
			}
			if !reflect.DeepEqual(cfg, config.Config{}) {
				t.Errorf("Load failed but returned %+v", cfg)
			}
		})
	}
}

// TestDecodeErrorsShowNoneOfTheFile reads files that do not decode, each
// holding a value in a setting, a secret's among them: the error names the
// file, line, column and key in fixed text, and holds nothing of the
// file's text, which go-toml's own messages quote.
func TestDecodeErrorsShowNoneOfTheFile(t *testing.T) {
	t.Parallel()

	const fixed = ": invalid: " + notDecoded
	// The value a key misses, on the lines after it, holds needle, or
	// these: the error must hold none of it, and name the key that misses
	// it.
	user, password := "someone-Uq7Zk", "hunter2-Pq7Zk"
	tests := []struct {
		name, file, want string
		value            []string // what of the file the error must not hold
		// crlf is whether the case holds as well with CRLF line endings,
		// the error at the same line and column.
		crlf bool
	}{
		{
			name:  "a secret on the line after its key",
			file:  "[keys]\nmain_key =\n\"" + needle + "\"\n",
			want:  "pisati.toml:2:11: keys.main_key" + fixed,
			value: []string{needle},
			crlf:  true,
		},
		{
			name: "a URL with a password, indented, on the line after its key",
			file: "[telemetry]\notlp_endpoint =\n  \"" + withUserInfo("https://", user, password, "collector.example.org") + "\"\n",
			want: "pisati.toml:2:16: telemetry.otlp_endpoint" + fixed,
			value: []string{
				user, password, user + ":" + password, "collector.example.org", "https",
			},
			crlf: true,
		},
		{
			name:  "a bare URL on the line after its key",
			file:  "[telemetry]\notlp_endpoint =\nhttp://" + needle + "\n",
			want:  "pisati.toml:2:16: telemetry.otlp_endpoint" + fixed,
			value: []string{"http", needle},
			crlf:  true,
		},
		{
			name:  "a string left open before the next key",
			file:  "[keys]\nmain_key = \"" + needle + "\nother = 1\n",
			want:  "pisati.toml:2:" + strconv.Itoa(13+len(needle)) + ": keys.main_key" + fixed,
			value: []string{needle, "other"},
			crlf:  true,
		},
		{
			name:  "a quoted key left open before the next",
			file:  "[keys]\n'" + needle + "\n'zz' = 1\n",
			want:  "pisati.toml:2:" + strconv.Itoa(2+len(needle)) + ": keys" + fixed,
			value: []string{needle, "zz"},
			crlf:  true,
		},
		{
			name:  "a secret after blank lines, in a string of many lines",
			file:  "[keys]\nmain_key =\n\n\n'''" + needle + "'''\n",
			want:  "pisati.toml:2:11: keys.main_key" + fixed,
			value: []string{needle},
			crlf:  true,
		},
		{
			name:  "a lone carriage return between two keys",
			file:  "[keys]\nmain_key_file = \"/run/key\"\n\r" + needle + " = 1\n",
			want:  "pisati.toml:3:1: keys" + fixed,
			value: []string{needle},
		},
		{
			name:  "a secret's key given twice",
			file:  "[keys]\nmain_key = \"" + needle + "\"\nmain_key = \"y-" + needle + "\"\n",
			want:  "pisati.toml:3:1: keys.main_key" + fixed,
			value: []string{needle},
			crlf:  true,
		},
		{
			name:  "a key given twice, once dotted",
			file:  "[log]\nlevel = \"debug\"\n[server]\nlisten = \":1\"\n[log]\nformat = \"text\"\n",
			want:  "pisati.toml:5:2: log" + fixed,
			value: []string{"debug", "text"},
			crlf:  true,
		},
		{
			name:  "a key given twice at the top, dotted",
			file:  "log.level = \"debug\"\nlog.level = \"warn\"\n",
			want:  "pisati.toml:2:1: log.level" + fixed,
			value: []string{"debug", "warn"},
			crlf:  true,
		},
		{
			name:  "an invalid escape inside a secret",
			file:  "[keys]\nmain_key = \"ЖЩ\\ЮЯ\"\n",
			want:  "pisati.toml:2:17: keys.main_key" + fixed,
			value: []string{"Ж", "Щ", "Ю", "Я", "Ð", "U+"},
		},
		{
			name:  "an invalid ASCII escape inside a secret",
			file:  "[keys]\nmain_key = \"pw\\qz\"\n",
			want:  "pisati.toml:2:15: keys.main_key" + fixed,
			value: []string{"'q'", "U+"},
		},
		{
			name:  "a bare float out of range",
			file:  "[keys]\nmain_key = 98765432109e999\n",
			want:  "pisati.toml:2:12: keys.main_key" + fixed,
			value: []string{"98765", "e999", "parsing", "strconv"},
		},
		{
			name:  "a bad character at the start of a value",
			file:  "[telemetry]\notlp_endpoint = Жhttp\n",
			want:  "pisati.toml:2:17: telemetry.otlp_endpoint" + fixed,
			value: []string{"Ж", "Ð", "U+", "http\n"},
		},
		{
			name:  "a bad ASCII character at the start of a value",
			file:  "[keys]\nmain_key = hunter2\n",
			want:  "pisati.toml:2:12: keys.main_key" + fixed,
			value: []string{"'h'", "U+", "hunter"},
		},
		{
			name:  "a bad character at the start of a key, in a table",
			file:  "[keys]\nЖmain_key = \"x\"\n",
			want:  "pisati.toml:2:1: keys" + fixed,
			value: []string{"Ж", "Ð", "U+"},
		},
		{
			name:  "a bad character at the start of a dotted key, at the top",
			file:  "keys.main_key = \"x\"\n!q = 1\n",
			want:  "pisati.toml:2:1" + fixed,
			value: []string{"'!'", "U+"},
		},
		{
			name:  "junk after a quoted key's value",
			file:  "[keys]\n\"main_key\" = \"pw\" Ж\n",
			want:  "pisati.toml:2:19: keys.main_key" + fixed,
			value: []string{"Ж", "Ð", "U+"},
		},
		{
			name:  "an invalid escape after a comment",
			file:  "[keys]\n# the key\nmain_key = \"pw\\qz\"\n",
			want:  "pisati.toml:3:15: keys.main_key" + fixed,
			value: []string{"'q'", "U+"},
		},
		{
			name:  "a control character in a comment before a key",
			file:  "[keys]\n# the \x01 key\nmain_key = \"pw\"\n",
			want:  "pisati.toml:2:7: keys" + fixed,
			value: []string{"U+", "\x01"},
		},
		{
			name:  "a bad value on a later line of an array",
			file:  "[server]\ntrusted_proxies = [ # a ] here closes nothing\n  \"10.0.0.0/8\",\n  Ж,\n]\n",
			want:  "pisati.toml:4:3: server.trusted_proxies" + fixed,
			value: []string{"Ж", "Ð", "U+"},
		},
		{
			name:  "an invalid escape in a string of many lines, under lines like a table and a comment",
			file:  "[keys]\nmain_key = \"\"\"\n[log]\n# a ' and a \"\n\\q\"\"\"\n",
			want:  "pisati.toml:5:1: keys.main_key" + fixed,
			value: []string{"'q'", "U+"},
		},
		{
			name:  "a bad value after a string of many lines and a table with a comment",
			file:  "[log]\nlevel = '''\n\"\"\"\n'''\n[keys] # the key\nmain_key = Жx\n",
			want:  "pisati.toml:6:12: keys.main_key" + fixed,
			value: []string{"Ж", "Ð", "U+"},
		},
		{
			name:  "a table name left open",
			file:  "[keys\n",
			want:  "pisati.toml:1:6: keys" + fixed,
			value: []string{"']'"},
		},
	}
	for _, tt := range tests {
		endings := []string{"\n"}
		if tt.crlf {
			endings = append(endings, "\r\n")
		}
		for _, ending := range endings {
			t.Run(fmt.Sprintf("%s, line endings %q", tt.name, ending), func(t *testing.T) {
				t.Parallel()

				_, err := setup{
					files: fstest.MapFS{"pisati.toml": tomlFile(strings.ReplaceAll(tt.file, "\n", ending))},
					env:   env{"PISATI_SERVER_BASE_URL": baseURL},
				}.load(t.Context())
				refused(t, err, config.ErrInvalid, tt.want)
				if !slices.ContainsFunc(errorParts(err), func(e error) bool { return e.Error() == tt.want }) {
					t.Errorf("error %q has no part that is exactly %q", err, tt.want)
				}
				for _, v := range tt.value {
					if strings.Contains(err.Error(), v) {
						t.Errorf("error %q holds %q, from the file", err, v)
					}
				}
			})
		}
	}
}

func TestLoadListsEveryError(t *testing.T) {
	t.Parallel()

	_, err := setup{
		files: fstest.MapFS{"pisati.toml": tomlFile("[server]\nbase_url = \"https://blog.example.org\"\nlisten = \"nope\"\n")},
		env:   env{"PISATI_LOG_LEVEL": "loud", "PISATI_SERVER_HSTS": "always"},
	}.load(t.Context())
	refused(t, err, config.ErrInvalid,
		"server.listen (PISATI_SERVER_LISTEN)", `"nope"`,
		"log.level (PISATI_LOG_LEVEL)", `"loud"`, "debug, info, warn or error",
		"server.hsts (PISATI_SERVER_HSTS)", `"always"`, "host-only, include-subdomains or preload",
	)
	joined, ok := errors.AsType[interface {
		error
		Unwrap() []error
	}](err)
	if !ok {
		t.Fatalf("error %q joins nothing", err)
	}
	if got := joined.Unwrap(); len(got) != 3 {
		t.Errorf("Load joined %d errors, want 3: %q", len(got), got)
	}
}

func TestLoadRefusesPlainHTTPBaseURL(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		in, want string
		https    bool
	}{
		{in: "https://blog.example.org", want: "https://blog.example.org", https: true},
		{in: "https://blog.example.org/", want: "https://blog.example.org", https: true},
		{in: "https://Blog.Example.ORG:443", want: "https://blog.example.org", https: true},
		{in: "HTTPS://blog.example.org:8443", want: "https://blog.example.org:8443", https: true},
		{in: "https://192.0.2.10", want: "https://192.0.2.10", https: true},
		{in: "https://[2001:db8::10]:8443", want: "https://[2001:db8::10]:8443", https: true},
		{in: "https://localhost", want: "https://localhost", https: true},
		{in: "http://localhost:8080", want: "http://localhost:8080"},
		{in: "http://LOCALHOST:8080/", want: "http://localhost:8080"},
		{in: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{in: "http://[::1]:8080", want: "http://[::1]:8080"},
		{in: "http://localhost:80", want: "http://localhost"},
	}
	for _, tt := range accepted {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			cfg, err := setup{env: env{"PISATI_SERVER_BASE_URL": tt.in}}.load(t.Context())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Server.BaseURL.String(); got != tt.want {
				t.Errorf("base URL %q, want %q", got, tt.want)
			}
			if got := cfg.Server.BaseURL.IsHTTPS(); got != tt.https {
				t.Errorf("IsHTTPS = %v, want %v", got, tt.https)
			}
			direct, err := config.ParseBaseURL(tt.in)
			if err != nil || direct != cfg.Server.BaseURL {
				t.Errorf("ParseBaseURL(%q) = %v, %v, want what Load made", tt.in, direct, err)
			}
		})
	}

	for _, in := range []string{
		"http://blog.example.org",
		"http://blog.example.org:8080",
		"http://127.0.0.2:8080",
		"http://[::ffff:127.0.0.1]:8080",
		"http://localhost.example.org",
		"http://0.0.0.0:8080",
		"https://blog.example.org/blog",
		"https://blog.example.org//",
		"https://blog.example.org/%2F",
		"https://blog.example.org?x=1",
		"https://blog.example.org?",
		"https://blog.example.org#top",
		"https://blog.example.org#",
		"https://someone@blog.example.org",
		withUserInfo("https://", "someone", "pw", "blog.example.org"),
		"ftp://blog.example.org",
		"blog.example.org",
		"//blog.example.org",
		"https:blog.example.org",
		"https://",
		"https:///",
		"https://blog example.org",
		"https://blog.example.org:0",
		"https://blog.example.org:65536",
		"https://[127.0.0.1]",
		"https://[fe80::1%25eth0]",
		"https://127.1",
		"https://-blog.example.org",
		"https://blog..example.org",
		"https://blog.example.org.",
		" https://blog.example.org",
		"https://blog.example.org\n",
	} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			_, err := setup{env: env{"PISATI_SERVER_BASE_URL": in}}.load(t.Context())
			refused(t, err, config.ErrInvalid, "server.base_url (PISATI_SERVER_BASE_URL)", "https")
			if _, err := config.ParseBaseURL(in); !errors.Is(err, config.ErrInvalid) {
				t.Errorf("ParseBaseURL(%q) = %v, want ErrInvalid", in, err)
			}
		})
	}
}

// withUserInfo returns the URL prefix, then user information of user and
// password, then rest: built from its parts, as the tests keep a URL that
// holds a password, so that no credential-shaped string sits in the
// source.
func withUserInfo(prefix, user, password, rest string) string {
	return prefix + user + ":" + password + "@" + rest
}

// urlKeys are the settings that hold a URL.
var urlKeys = []string{"server.base_url", "telemetry.otlp_endpoint", "dev.vite_url"}

// tomlString spells s as a TOML basic string, escaping what TOML does not
// let one hold as it is.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// givenIn is where a test gives a setting: its variable or pisati.toml.
type givenIn uint8

const (
	inVariable givenIn = iota
	inFile
)

// everywhere is each place a setting may be given.
var everywhere = []givenIn{inVariable, inFile}

func (g givenIn) String() string {
	switch g {
	case inVariable:
		return "in its variable"
	case inFile:
		return "in pisati.toml"
	}
	panic(fmt.Sprintf("givenIn(%d) is not a place a setting is given", g))
}

// loadURLSetting loads the URL setting key set to value, given where
// says, with a base URL on localhost unless key is the base URL, and
// returns what Load returns and the setting's label.
func loadURLSetting(t *testing.T, key, value string, where givenIn) (cfg config.Config, label string, err error) {
	t.Helper()
	section, name, _ := strings.Cut(key, ".")
	variable := "PISATI_" + strings.ToUpper(section+"_"+name)
	s := setup{env: env{}}
	if key != "server.base_url" {
		s.env["PISATI_SERVER_BASE_URL"] = "http://localhost:8080"
	}
	switch where {
	case inVariable:
		s.env[variable] = value
	case inFile:
		s.files = fstest.MapFS{"pisati.toml": tomlFile("[" + section + "]\n" + name + " = " + tomlString(value) + "\n")}
	default:
		t.Fatalf("givenIn(%d): no such place to give a setting", where)
	}
	cfg, err = s.load(t.Context())
	return cfg, key + " (" + variable + ")", err
}

// TestURLSettingsRefuseUnescapedCharacters holds every URL setting to
// RFC 3986: a character a URL holds only percent-encoded is refused, from
// a variable or a file, in an error that names the setting and says why
// but does not repeat the value; the percent-encoded form is accepted.
func TestURLSettingsRefuseUnescapedCharacters(t *testing.T) {
	t.Parallel()

	const (
		encode  = "a character a URL must percent-encode"
		control = "a control character"
	)
	tests := []struct {
		name, value, reason string
	}{
		// What fuzzing found: a path of one space.
		{"a path of one space", "http://A/ ", encode},
		{"a space", "http://localhost:5173/pa th-q7z", encode},
		{"a tab", "http://localhost:5173/pa\tth-q7z", control},
		{"a NUL", "http://localhost:5173/pa\x00th-q7z", control},
		{"a newline", "http://localhost:5173/pa\nth-q7z", control},
		{"DEL", "http://localhost:5173/pa\x7fth-q7z", control},
		{"a double quote", `http://localhost:5173/pa"th-q7z`, encode},
		{"a less-than sign", "http://localhost:5173/pa<th-q7z", encode},
		{"a greater-than sign", "http://localhost:5173/pa>th-q7z", encode},
		{"a backslash", `http://localhost:5173/pa\th-q7z`, encode},
		{"a caret", "http://localhost:5173/pa^th-q7z", encode},
		{"a grave accent", "http://localhost:5173/pa`th-q7z", encode},
		{"a brace", "http://localhost:5173/pa{th}-q7z", encode},
		{"a vertical bar", "http://localhost:5173/pa|th-q7z", encode},
		{"a bracket", "http://localhost:5173/pa[th]-q7z", encode},
		{"a letter outside ASCII", "http://localhost:5173/paéth-q7z", encode},
		{"a byte outside UTF-8", "http://localhost:5173/pa\xffth-q7z", encode},
	}
	for _, key := range urlKeys {
		for _, tt := range tests {
			for _, where := range everywhere {
				if where == inFile && !utf8.ValidString(tt.value) {
					continue // a TOML file is UTF-8, so it cannot hold this value
				}
				t.Run(fmt.Sprintf("%s, %s, %v", key, tt.name, where), func(t *testing.T) {
					t.Parallel()

					_, label, err := loadURLSetting(t, key, tt.value, where)
					refused(t, err, config.ErrInvalid, label+": invalid", tt.reason)
					if strings.Contains(err.Error(), tt.value) || strings.Contains(err.Error(), "q7z") {
						t.Errorf("error %q repeats the value", err)
					}
				})
			}
		}
	}

	accepted := []struct{ value, want string }{
		{"http://LOCALHOST/%20", "http://localhost/%20"},
		{"http://localhost:5173/pa%20th%09%22%5B%5D%7C%C3%A9", "http://localhost:5173/pa%20th%09%22%5B%5D%7C%C3%A9"},
		{"http://localhost:5173/a-._~!$&'()*+,;=:@b/%2F", "http://localhost:5173/a-._~!$&'()*+,;=:@b/%2F"},
	}
	for _, key := range []string{"telemetry.otlp_endpoint", "dev.vite_url"} {
		for _, tt := range accepted {
			for _, where := range everywhere {
				t.Run(fmt.Sprintf("%s, %s, %v", key, tt.value, where), func(t *testing.T) {
					t.Parallel()

					cfg, _, err := loadURLSetting(t, key, tt.value, where)
					if err != nil {
						t.Fatalf("Load: %v", err)
					}
					got := map[string]config.OptionalURL{"telemetry.otlp_endpoint": cfg.Telemetry.OTLPEndpoint, "dev.vite_url": cfg.Dev.ViteURL}[key]
					if s := optional(got); s != tt.want {
						t.Errorf("%s = %q, want %q", key, s, tt.want)
					}
				})
			}
		}
	}
}

// TestURLErrorsHideUserInformation refuses a URL setting's value that
// holds user information, from a variable or a file, in an error that
// names the setting and says why; and no error for any URL setting's
// value holding a password, whatever else is wrong with it, shows the
// password.
func TestURLErrorsHideUserInformation(t *testing.T) {
	t.Parallel()

	const (
		secret   = "pw-q7Zx9kP"
		userInfo = "it holds user information"
	)
	tests := []struct {
		value, reason string
	}{
		{withUserInfo("http://", "someone", secret, "localhost:4318"), userInfo},
		{withUserInfo("https://", "someone", secret, "blog.example.org/v1"), userInfo},
		{"http://" + secret + "@localhost", userInfo},
		{withUserInfo("http://", "someone", secret, ""), ""},
		{withUserInfo("http://", "someone", secret, "localhost:0"), ""},
		{withUserInfo("http://", "someone", secret, "localhost/pa th"), ""},
		{withUserInfo("http://", "someone", secret, "localhost/?x=1"), ""},
		{withUserInfo("http://", "someone", secret, "localhost#top"), ""},
		{withUserInfo("http://", "someone", secret+"^", "localhost"), ""},
		{withUserInfo("http://", "someone", secret, "local\thost"), ""},
		{withUserInfo("ftp://", "someone", secret, "localhost"), ""},
		{withUserInfo("", "someone", secret, "localhost"), ""},
		{withUserInfo("//", "someone", secret, "localhost"), ""},
	}
	for _, key := range urlKeys {
		for _, tt := range tests {
			for _, where := range everywhere {
				t.Run(fmt.Sprintf("%s=%q, %v", key, tt.value, where), func(t *testing.T) {
					t.Parallel()

					_, label, err := loadURLSetting(t, key, tt.value, where)
					refused(t, err, config.ErrInvalid, label+": invalid", tt.reason)
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error %q shows the password", err)
					}
				})
			}
		}
	}
}

func TestSettingRules(t *testing.T) {
	t.Parallel()

	listen := func(c config.Config) string { return c.Server.Listen.String() }
	proxies := func(c config.Config) string { return fmt.Sprint(c.Server.TrustedProxies) }
	hsts := func(c config.Config) string { return c.Server.HSTS.String() }
	level := func(c config.Config) string { return c.Log.Level.String() }
	format := func(c config.Config) string { return c.Log.Format.String() }
	pprof := func(c config.Config) string { return c.Telemetry.Pprof.String() }
	otlp := func(c config.Config) string { return optional(c.Telemetry.OTLPEndpoint) }

	tests := []struct {
		env, value string
		got        func(config.Config) string
		want       string // empty when the value is refused
	}{
		{env: "PISATI_SERVER_LISTEN", value: "127.0.0.1:9000", got: listen, want: "127.0.0.1:9000"},
		{env: "PISATI_SERVER_LISTEN", value: "[::1]:9000", got: listen, want: "[::1]:9000"},
		{env: "PISATI_SERVER_LISTEN", value: "localhost:9000", got: listen, want: "localhost:9000"},
		{env: "PISATI_SERVER_LISTEN", value: ":65535", got: listen, want: ":65535"},
		{env: "PISATI_SERVER_LISTEN", value: ":0", got: listen},
		{env: "PISATI_SERVER_LISTEN", value: ":65536", got: listen},
		{env: "PISATI_SERVER_LISTEN", value: ":http", got: listen},
		{env: "PISATI_SERVER_LISTEN", value: ":-1", got: listen},
		{env: "PISATI_SERVER_LISTEN", value: "8080", got: listen},
		{env: "PISATI_SERVER_LISTEN", value: "", got: listen},
		{env: "PISATI_SERVER_LISTEN", value: "::1:8080", got: listen},
		{env: "PISATI_SERVER_LISTEN", value: "bad host:8080", got: listen},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "10.0.0.1", got: proxies, want: "[10.0.0.1/32]"},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "2001:db8::1", got: proxies, want: "[2001:db8::1/128]"},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "10.0.0.0/8,192.168.0.0/16", got: proxies, want: "[10.0.0.0/8 192.168.0.0/16]"},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "", got: proxies, want: "[]"},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "10.0.0.1/8", got: proxies},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "10.0.0.0/33", got: proxies},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "10.0.0.0/8,,192.168.0.0/16", got: proxies},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "fe80::1%eth0", got: proxies},
		{env: "PISATI_SERVER_TRUSTED_PROXIES", value: "proxy.example.org", got: proxies},
		{env: "PISATI_SERVER_HSTS", value: "preload", got: hsts, want: "preload"},
		{env: "PISATI_SERVER_HSTS", value: "Preload", got: hsts},
		{env: "PISATI_LOG_LEVEL", value: "debug", got: level, want: "DEBUG"},
		{env: "PISATI_LOG_LEVEL", value: "warn", got: level, want: "WARN"},
		{env: "PISATI_LOG_LEVEL", value: "INFO", got: level},
		{env: "PISATI_LOG_LEVEL", value: "info+2", got: level},
		{env: "PISATI_LOG_FORMAT", value: "text", got: format, want: "text"},
		{env: "PISATI_LOG_FORMAT", value: "yaml", got: format},
		{env: "PISATI_TELEMETRY_PPROF", value: "on", got: pprof, want: "on"},
		{env: "PISATI_TELEMETRY_PPROF", value: "true", got: pprof},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "http://collector:4318", got: otlp, want: "http://collector:4318"},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "https://Collector.Example.org/v1/traces", got: otlp, want: "https://collector.example.org/v1/traces"},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "", got: otlp, want: "unset"},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "ftp://collector", got: otlp},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "collector:4318", got: otlp},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: withUserInfo("http://", "someone", "pw", "collector.example.org"), got: otlp},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "http://collector#x", got: otlp},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "http://collector?x=1", got: otlp},
		{env: "PISATI_TELEMETRY_OTLP_ENDPOINT", value: "http:///v1/traces", got: otlp},
	}
	for _, tt := range tests {
		t.Run(tt.env+"="+tt.value, func(t *testing.T) {
			t.Parallel()

			cfg, err := setup{env: env{"PISATI_SERVER_BASE_URL": baseURL, tt.env: tt.value}}.load(t.Context())
			if tt.want == "" {
				refused(t, err, config.ErrInvalid, tt.env)
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tt.got(cfg); got != tt.want {
				t.Errorf("%s=%q gives %q, want %q", tt.env, tt.value, got, tt.want)
			}
		})
	}
}

func TestStateDirFromSystemd(t *testing.T) {
	t.Parallel()

	const systemd = "/var/lib/private/pisati"
	stateFile := fstest.MapFS{"pisati.toml": tomlFile("[storage]\nstate_dir = \"/srv/pisati\"\n")}

	accepted := map[string]struct {
		s    setup
		want string
	}{
		"from systemd":          {s: setup{state: systemd}, want: systemd},
		"the default":           {s: setup{}, want: "/var/lib/pisati"},
		"a file wins":           {s: setup{state: systemd, files: stateFile}, want: "/srv/pisati"},
		"a variable wins":       {s: setup{state: systemd, files: stateFile, env: env{"PISATI_STORAGE_STATE_DIR": "/srv/env"}}, want: "/srv/env"},
		"unused when overruled": {s: setup{state: "relative", env: env{"PISATI_STORAGE_STATE_DIR": "/srv/env"}}, want: "/srv/env"},
	}
	for name, tt := range accepted {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.s.env == nil {
				tt.s.env = env{}
			}
			tt.s.env["PISATI_SERVER_BASE_URL"] = baseURL
			cfg, err := tt.s.load(t.Context())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Storage.StateDir != tt.want {
				t.Errorf("state directory %q, want %q", cfg.Storage.StateDir, tt.want)
			}
		})
	}

	refusals := map[string]struct {
		s    setup
		name string
	}{
		"relative from systemd": {s: setup{state: "pisati"}, name: "STATE_DIRECTORY"},
		"several from systemd":  {s: setup{state: "/var/lib/a:/var/lib/b"}, name: "STATE_DIRECTORY"},
		"unclean from systemd":  {s: setup{state: "/var/lib/../lib/pisati"}, name: "STATE_DIRECTORY"},
		"relative in a file": {
			s:    setup{files: fstest.MapFS{"pisati.toml": tomlFile("[storage]\nstate_dir = \"data\"\n")}},
			name: "storage.state_dir (PISATI_STORAGE_STATE_DIR)",
		},
		"unclean in a variable": {
			s:    setup{env: env{"PISATI_STORAGE_STATE_DIR": "/srv/pisati/"}},
			name: "storage.state_dir (PISATI_STORAGE_STATE_DIR)",
		},
	}
	for name, tt := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.s.env == nil {
				tt.s.env = env{}
			}
			tt.s.env["PISATI_SERVER_BASE_URL"] = baseURL
			_, err := tt.s.load(t.Context())
			refused(t, err, config.ErrInvalid, tt.name)
		})
	}
}

func TestMainKeySourceOrder(t *testing.T) {
	t.Parallel()

	key := &fstest.MapFile{Data: bytes.Repeat([]byte{7}, 32), Mode: 0o400}
	withKey := fstest.MapFS{"pisati-main-key": key, "other": key}
	keyFile := fstest.MapFS{"pisati.toml": tomlFile("[keys]\nmain_key_file = \"/etc/pisati/main.key\"\n")}

	tests := map[string]struct {
		s    setup
		want string
	}{
		"nothing given":       {s: setup{}, want: "generated"},
		"another credential":  {s: setup{creds: fstest.MapFS{"other": key}}, want: "generated"},
		"the credential":      {s: setup{creds: withKey}, want: "credential pisati-main-key"},
		"a file setting wins": {s: setup{creds: withKey, files: keyFile}, want: "file /etc/pisati/main.key"},
		"a variable wins": {
			s:    setup{creds: withKey, files: keyFile, env: env{"PISATI_KEYS_MAIN_KEY_FILE": "/run/secrets/main.key"}},
			want: "file /run/secrets/main.key",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.s.env == nil {
				tt.s.env = env{}
			}
			tt.s.env["PISATI_SERVER_BASE_URL"] = baseURL
			cfg, err := tt.s.load(t.Context())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := source(cfg.Keys.MainKey); got != tt.want {
				t.Errorf("main key from %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDevViteURLOnlyWithLocalhost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, base, vite, want string // want is empty when refused
	}{
		{name: "localhost", base: "http://localhost:8080", vite: "http://localhost:5173", want: "http://localhost:5173"},
		{name: "loopback", base: "https://127.0.0.1:8443", vite: "http://127.0.0.1:5173", want: "http://127.0.0.1:5173"},
		{name: "ipv6 loopback", base: "http://[::1]:8080", vite: "https://[::1]:5173", want: "https://[::1]:5173"},
		{name: "unset", base: baseURL, vite: "", want: "unset"},
		{name: "a public base URL", base: baseURL, vite: "http://localhost:5173"},
		{name: "a public Vite URL", base: "http://localhost:8080", vite: "http://vite.example.org:5173"},
		{name: "another loopback address", base: "http://localhost:8080", vite: "http://127.0.0.2:5173"},
		{name: "a public Vite URL with a path", base: "http://localhost:8080", vite: "https://vite.example.org:5173/pa-q7z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := env{"PISATI_SERVER_BASE_URL": tt.base, "PISATI_DEV_VITE_URL": tt.vite}
			if strings.HasPrefix(tt.vite, "https:") {
				// An https Vite dev server needs its certificate and key.
				e["PISATI_DEV_VITE_CERT_FILE"], e["PISATI_DEV_VITE_KEY_FILE"] = "/certs/localhost.pem", "/certs/localhost-key.pem"
			}
			cfg, err := setup{env: e}.load(t.Context())
			if tt.want == "" {
				refused(t, err, config.ErrInvalid, "dev.vite_url (PISATI_DEV_VITE_URL)", "localhost")
				// The error names the setting and the rule, never the URL.
				_, hostAndPath, _ := strings.Cut(tt.vite, "://")
				if strings.Contains(err.Error(), hostAndPath) || strings.Contains(err.Error(), "q7z") {
					t.Errorf("error %q repeats the URL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := optional(cfg.Dev.ViteURL); got != tt.want {
				t.Errorf("Vite URL %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDevViteCertificate(t *testing.T) {
	t.Parallel()

	const (
		cert = "/home/dev/certs/localhost.pem"
		key  = "/home/dev/certs/localhost-key.pem"
	)
	tests := []struct {
		name               string
		vite, cert, key    *string
		wantCert, wantKey  string
		missing, malformed []string // the settings refused, by label
	}{
		{name: "https, with both", vite: new("https://localhost:5173"), cert: new(cert), key: new(key), wantCert: cert, wantKey: key},
		{
			name: "https, with no key", vite: new("https://localhost:5173"), cert: new(cert),
			missing: []string{"dev.vite_key_file (PISATI_DEV_VITE_KEY_FILE)"},
		},
		{
			name: "https, with neither", vite: new("https://[::1]:5173"),
			missing: []string{"dev.vite_cert_file (PISATI_DEV_VITE_CERT_FILE)", "dev.vite_key_file (PISATI_DEV_VITE_KEY_FILE)"},
		},
		// Used only with an https dev.vite_url, and otherwise accepted
		// and left unused, as server.hsts is with a plain-http base URL.
		{name: "http, with both", vite: new("http://localhost:5173"), cert: new(cert), key: new(key), wantCert: cert, wantKey: key},
		{name: "no dev server, with both", cert: new(cert), key: new(key), wantCert: cert, wantKey: key},
		{name: "http, with neither", vite: new("http://localhost:5173")},
		{
			name: "a relative path", vite: new("https://localhost:5173"), cert: new("certs/localhost.pem"), key: new(key),
			malformed: []string{"dev.vite_cert_file (PISATI_DEV_VITE_CERT_FILE)"},
		},
		{
			name: "an unclean path, with no dev server", cert: new(cert), key: new("/home/dev/../dev/key.pem"),
			malformed: []string{"dev.vite_key_file (PISATI_DEV_VITE_KEY_FILE)"},
		},
		{
			name: "empty", vite: new("https://localhost:5173"), cert: new(""), key: new(key),
			malformed: []string{"dev.vite_cert_file (PISATI_DEV_VITE_CERT_FILE)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := env{"PISATI_SERVER_BASE_URL": "http://localhost:8080"}
			for name, v := range map[string]*string{
				"PISATI_DEV_VITE_URL": tt.vite, "PISATI_DEV_VITE_CERT_FILE": tt.cert, "PISATI_DEV_VITE_KEY_FILE": tt.key,
			} {
				if v != nil {
					e[name] = *v
				}
			}
			cfg, err := setup{env: e}.load(t.Context())
			switch {
			case len(tt.missing) > 0:
				refused(t, err, config.ErrMissing, tt.missing...)
				return
			case len(tt.malformed) > 0:
				refused(t, err, config.ErrInvalid, tt.malformed...)
				// A key pasted in place of its path stays out of the error.
				for _, v := range []*string{tt.cert, tt.key} {
					if v != nil && *v != "" && *v != cert && *v != key && strings.Contains(err.Error(), *v) {
						t.Errorf("error %q repeats the value %q", err, *v)
					}
				}
				return
			case err != nil:
				t.Fatalf("Load: %v", err)
			}
			if cfg.Dev.ViteCertFile != tt.wantCert || cfg.Dev.ViteKeyFile != tt.wantKey {
				t.Errorf("certificate %q and key %q, want %q and %q", cfg.Dev.ViteCertFile, cfg.Dev.ViteKeyFile, tt.wantCert, tt.wantKey)
			}
		})
	}

	t.Run("from a file", func(t *testing.T) {
		t.Parallel()

		cfg, err := setup{files: fstest.MapFS{"pisati.toml": tomlFile(
			"[server]\nbase_url = \"https://localhost:8443\"\n[dev]\nvite_url = \"https://localhost:5173\"\n" +
				"vite_cert_file = \"" + cert + "\"\nvite_key_file = \"" + key + "\"\n")}}.load(t.Context())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Dev.ViteCertFile != cert || cfg.Dev.ViteKeyFile != key {
			t.Errorf("certificate %q and key %q, want %q and %q", cfg.Dev.ViteCertFile, cfg.Dev.ViteKeyFile, cert, key)
		}
	})
}

// devCertificatePath is one case of testdata/dev-certificate-paths.json,
// the table vite.config.test.ts reads too, so that Pisati and Vite agree
// on what a certificate or key path may be.
type devCertificatePath struct {
	Path  string `json:"path"`
	Valid bool   `json:"valid"`
}

// TestDevCertificatePaths checks each path of the shared table in either
// variable, with an https, an http or no dev server: a valid one is
// accepted, and used only with https; any other is refused whatever the
// dev server, without its value in the error.
func TestDevCertificatePaths(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/dev-certificate-paths.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cases []devCertificatePath
	if err := dec.Decode(&cases); err != nil || len(cases) == 0 {
		t.Fatalf("testdata/dev-certificate-paths.json: %v, %d cases", err, len(cases))
	}
	const other = "/home/dev/certs/other.pem"
	for _, c := range cases {
		for _, vite := range []string{"https://localhost:5173", "http://localhost:5173", ""} {
			for _, name := range []string{"PISATI_DEV_VITE_CERT_FILE", "PISATI_DEV_VITE_KEY_FILE"} {
				t.Run(fmt.Sprintf("%s %q with dev server %q", name, c.Path, vite), func(t *testing.T) {
					t.Parallel()

					e := env{
						"PISATI_SERVER_BASE_URL": "http://localhost:8080", "PISATI_DEV_VITE_URL": vite,
						"PISATI_DEV_VITE_CERT_FILE": other, "PISATI_DEV_VITE_KEY_FILE": other,
					}
					e[name] = c.Path
					cfg, err := setup{env: e}.load(t.Context())
					if !c.Valid {
						refused(t, err, config.ErrInvalid, name)
						if c.Path != "" && strings.Contains(err.Error(), c.Path) {
							t.Errorf("error %q repeats the value %q", err, c.Path)
						}
						return
					}
					if err != nil {
						t.Fatalf("Load: %v", err)
					}
					if got := map[string]string{
						"PISATI_DEV_VITE_CERT_FILE": cfg.Dev.ViteCertFile, "PISATI_DEV_VITE_KEY_FILE": cfg.Dev.ViteKeyFile,
					}[name]; got != c.Path {
						t.Errorf("%s gives %q, want %q", name, got, c.Path)
					}
				})
			}
		}
	}
}

// devViteURL is one case of testdata/dev-vite-urls.json, the table
// vite.config.test.ts reads too, so that Pisati and Vite refuse the same
// dev.vite_url values. A case gives its URL whole, or, when it holds user
// information, in parts: the scheme, the user, the password if there is
// one, and the host, so that no credential-shaped string sits in the
// table. Base is the base URL beside it, http://localhost:8080 unless it
// says otherwise; a case whose verdict hangs on it is PisatiOnly, as Vite
// never reads the base URL.
type devViteURL struct {
	URL        string  `json:"url"`
	Scheme     string  `json:"scheme"`
	User       *string `json:"user"`
	Password   *string `json:"password"`
	Host       string  `json:"host"`
	Base       string  `json:"base"`
	PisatiOnly bool    `json:"pisatiOnly"`
	Valid      bool    `json:"valid"`
}

// value returns the URL c gives, assembled from its parts when it has
// them, and whether c is well formed: a URL whole or in parts, not both.
func (c devViteURL) value() (string, bool) {
	if c.User == nil {
		return c.URL, c.Scheme == "" && c.Password == nil && c.Host == ""
	}
	password := ""
	if c.Password != nil {
		password = ":" + *c.Password
	}
	return c.Scheme + "://" + *c.User + password + "@" + c.Host, c.URL == "" && c.Scheme != "" && c.Host != ""
}

// TestDevViteURLs checks each URL of the shared table as dev.vite_url,
// beside its base URL and with an https dev server's certificate and key
// given: a valid one is accepted, any other refused without its value in
// the error.
func TestDevViteURLs(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/dev-vite-urls.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cases []devViteURL
	if err := dec.Decode(&cases); err != nil || len(cases) == 0 {
		t.Fatalf("testdata/dev-vite-urls.json: %v, %d cases", err, len(cases))
	}
	for _, c := range cases {
		url, ok := c.value()
		if !ok {
			t.Fatalf("testdata/dev-vite-urls.json: case %+v gives its URL both whole and in parts, or neither", c)
		}
		t.Run(fmt.Sprintf("%q beside %q", url, c.Base), func(t *testing.T) {
			t.Parallel()

			_, err := setup{env: env{
				"PISATI_SERVER_BASE_URL": cmp.Or(c.Base, "http://localhost:8080"), "PISATI_DEV_VITE_URL": url,
				"PISATI_DEV_VITE_CERT_FILE": "/certs/localhost.pem", "PISATI_DEV_VITE_KEY_FILE": "/certs/localhost-key.pem",
			}}.load(t.Context())
			if c.Valid {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			refused(t, err, config.ErrInvalid, "dev.vite_url (PISATI_DEV_VITE_URL): invalid")
			if url != "" && strings.Contains(err.Error(), url) || c.Password != nil && strings.Contains(err.Error(), *c.Password) {
				t.Errorf("error %q repeats the value", err)
			}
		})
	}
}

func TestBaseURLIsLocal(t *testing.T) {
	t.Parallel()

	for base, want := range map[string]bool{
		"http://localhost:8080":     true,
		"http://127.0.0.1":          true,
		"http://[::1]:8080":         true,
		"https://LocalHost:8443/":   true,
		"https://blog.example.org":  false,
		"https://127.0.0.2":         false,
		"https://[::2]":             false,
		"https://localhost.example": false,
	} {
		b, err := config.ParseBaseURL(base)
		if err != nil {
			t.Fatalf("ParseBaseURL(%q): %v", base, err)
		}
		if got := b.IsLocal(); got != want {
			t.Errorf("%q.IsLocal() = %v, want %v", base, got, want)
		}
	}
	if (config.BaseURL{}).IsLocal() {
		t.Error("the zero BaseURL is local")
	}
}

func TestDevViteOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		vite, origin, host string
		https, local       bool
	}{
		{vite: "http://localhost:5173", origin: "http://localhost:5173", host: "localhost:5173", local: true},
		{vite: "http://LOCALHOST:5173/static/", origin: "http://localhost:5173", host: "localhost:5173", local: true},
		{vite: "http://127.0.0.1:5173", origin: "http://127.0.0.1:5173", host: "127.0.0.1:5173", local: true},
		{vite: "https://[::1]:5173", origin: "https://[::1]:5173", host: "[::1]:5173", https: true, local: true},
		{vite: "http://localhost:80", origin: "http://localhost", host: "localhost", local: true},
		{vite: "https://vite.example.org:443", origin: "https://vite.example.org", host: "vite.example.org", https: true},
		{vite: "http://127.0.0.2:5173", origin: "http://127.0.0.2:5173", host: "127.0.0.2:5173"},
	}
	for _, tt := range tests {
		u, err := config.ParseOptionalURL(tt.vite)
		if err != nil {
			t.Fatalf("ParseOptionalURL(%q): %v", tt.vite, err)
		}
		o, ok := config.Dev{ViteURL: u}.ViteOrigin()
		if !ok || o.String() != tt.origin || o.Host() != tt.host || o.IsHTTPS() != tt.https || o.IsLocal() != tt.local {
			t.Errorf("ViteOrigin for %q = %q, host %q, https %v, local %v, %v; want %q, %q, %v, %v, true",
				tt.vite, o.String(), o.Host(), o.IsHTTPS(), o.IsLocal(), ok, tt.origin, tt.host, tt.https, tt.local)
		}
	}
	if o, ok := (config.Dev{}).ViteOrigin(); ok || o != (config.Origin{}) || o.String() != "" {
		t.Errorf("ViteOrigin with no Vite URL = %v, %v; want the zero Origin, false", o, ok)
	}
}

// brokenFS fails to open anything, with err.
type brokenFS struct{ err error }

func (b brokenFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: b.err}
}

func TestSourcesThatCannotBeRead(t *testing.T) {
	t.Parallel()

	base := env{"PISATI_SERVER_BASE_URL": baseURL}

	// None of these is told the base URL: a file that cannot be read may
	// hold it, so it is not reported missing.
	t.Run("files", func(t *testing.T) {
		t.Parallel()

		src := env{}.sources()
		src.Files = brokenFS{err: fs.ErrPermission}
		_, err := config.Load(t.Context(), src, "")
		refused(t, err, fs.ErrPermission, "pisati.toml")
		if errors.Is(err, config.ErrMissing) {
			t.Errorf("error %q reports a setting missing", err)
		}
	})

	t.Run("credentials", func(t *testing.T) {
		t.Parallel()

		src := base.sources()
		src.Credentials = brokenFS{err: fs.ErrPermission}
		_, err := config.Load(t.Context(), src, "")
		refused(t, err, fs.ErrPermission, "pisati-main-key")
	})

	for name, files := range map[string]fstest.MapFS{
		"pisati.toml is a directory": {"pisati.toml/inner": tomlFile("")},
		"a drop-in is a directory":   {"pisati.toml.d/10-dir.toml/inner": tomlFile("")},
		"pisati.toml.d is a file":    {"pisati.toml.d": tomlFile("")},
		"too large":                  {"pisati.toml": {Data: bytes.Repeat([]byte("#"), 1<<20+1)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := setup{files: files}.load(t.Context())
			if err == nil {
				t.Fatal("Load succeeded")
			}
			if !strings.Contains(err.Error(), "pisati.toml") {
				t.Errorf("error %q does not name the file", err)
			}
			if errors.Is(err, config.ErrMissing) {
				t.Errorf("error %q reports a setting missing", err)
			}
		})
	}
}

// watchedFS records each name opened from fs, after calling before with
// it.
type watchedFS struct {
	fs     fstest.MapFS
	before func(name string)
	opened []string
}

func (w *watchedFS) Open(name string) (fs.File, error) {
	w.before(name)
	w.opened = append(w.opened, name)
	return w.fs.Open(name)
}

func TestLoadStopsWhenCancelled(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"pisati.toml":              tomlFile("[server]\nbase_url = \"https://blog.example.org\"\n"),
		"pisati.toml.d/10-a.toml":  tomlFile("[log]\nlevel = \"debug\"\n"),
		"pisati.toml.d/20-b.toml":  tomlFile("[log]\nformat = \"text\"\n"),
		"pisati.toml.d/README.txt": tomlFile("not read"),
	}
	key := fstest.MapFS{"pisati-main-key": tomlFile("key")}

	// Each case cancels when Load opens the file named, and must stop
	// before reading anything after it.
	tests := map[string]struct {
		cancelAt  string // "" cancels before Load starts
		wantFiles []string
	}{
		"before it starts":      {cancelAt: "", wantFiles: nil},
		"after pisati.toml":     {cancelAt: "pisati.toml", wantFiles: []string{"pisati.toml"}},
		"after the first entry": {cancelAt: "pisati.toml.d/10-a.toml", wantFiles: []string{"pisati.toml", "pisati.toml.d", "pisati.toml.d/10-a.toml"}},
		"after the last file":   {cancelAt: "pisati.toml.d/20-b.toml", wantFiles: []string{"pisati.toml", "pisati.toml.d", "pisati.toml.d/10-a.toml", "pisati.toml.d/20-b.toml"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelAt == "" {
				cancel()
			}
			cancelAt := func(name string) {
				if name == tt.cancelAt {
					cancel()
				}
			}
			watched := &watchedFS{fs: files, before: cancelAt}
			creds := &watchedFS{fs: key, before: cancelAt}
			src := env{}.sources()
			src.Files, src.Credentials = watched, creds

			cfg, err := config.Load(ctx, src, "")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Load = %v, want an error wrapping context.Canceled", err)
			}
			if !reflect.DeepEqual(cfg, config.Config{}) {
				t.Errorf("Load was cancelled but returned %+v", cfg)
			}
			if !slices.Equal(watched.opened, tt.wantFiles) {
				t.Errorf("Load opened %q, want %q", watched.opened, tt.wantFiles)
			}
			if len(creds.opened) != 0 {
				t.Errorf("Load looked up credentials %q after it was cancelled", creds.opened)
			}
		})
	}

	t.Run("not cancelled", func(t *testing.T) {
		t.Parallel()

		src := env{}.sources()
		src.Files = &watchedFS{fs: files, before: func(string) {}}
		src.Credentials = &watchedFS{fs: key, before: func(string) {}}
		cfg, err := config.Load(t.Context(), src, "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Log.Level != slog.LevelDebug || cfg.Log.Format != config.LogText || source(cfg.Keys.MainKey) != "credential pisati-main-key" {
			t.Errorf("Load read %v, %v and %s, want every file and the credential", cfg.Log.Level, cfg.Log.Format, source(cfg.Keys.MainKey))
		}
	})
}

func TestSecretRedacts(t *testing.T) {
	t.Parallel()

	const value = "correct-horse-battery-staple"
	in := []byte(value)
	s, err := config.NewSecret(in)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	in[0] = 'X'
	if got := string(s.Reveal()); got != value {
		t.Errorf("Reveal = %q after the caller changed its slice, want %q", got, value)
	}
	revealed := s.Reveal()
	revealed[0] = 'Y'
	if got := string(s.Reveal()); got != value {
		t.Errorf("Reveal = %q after a caller changed what it revealed, want %q", got, value)
	}

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("loaded", "key", s)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("loaded", "key", s)
	if !strings.Contains(logs.String(), `"key":"[redacted]"`) || !strings.Contains(logs.String(), "key=[redacted]") {
		t.Errorf("logs %q do not show [redacted]", logs.String())
	}

	// A secret held in another value's unexported field is out of fmt's
	// reach for methods, so the bytes themselves must not print.
	type holder struct{ key config.Secret }
	printed := []string{logs.String()}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%o", "%b", "%c", "%U"} {
		printed = append(printed, fmt.Sprintf(verb, s), fmt.Sprintf(verb, holder{key: s}), fmt.Sprintf(verb, []config.Secret{s}))
	}
	for _, out := range printed {
		for _, leak := range []string{value, fmt.Sprintf("%x", value), fmt.Sprintf("%d", []byte(value))[1:20]} {
			if strings.Contains(out, leak) {
				t.Errorf("%q shows the secret as %q", out, leak)
			}
		}
	}

	for _, empty := range [][]byte{nil, {}} {
		if _, err := config.NewSecret(empty); !errors.Is(err, config.ErrInvalid) {
			t.Errorf("NewSecret(%v) = %v, want ErrInvalid", empty, err)
		}
	}
	var zero config.Secret
	if got := zero.Reveal(); len(got) != 0 {
		t.Errorf("the zero Secret reveals %d bytes", len(got))
	}
}

func FuzzLoad(f *testing.F) {
	f.Add(everyKey, "[log]\nlevel = \"debug\"\n", "", "", false)
	f.Add("", "", "PISATI_SERVER_BASE_URL=http://localhost:8080\nPISATI_DEV_VITE_URL=http://localhost:5173", "/var/lib/private/pisati", true)
	f.Add("[dev]\nvite_url = \"https://localhost:5173\"\nvite_cert_file = \"/c.pem\"\n", "", "PISATI_SERVER_BASE_URL=http://[::1]:8080\nPISATI_DEV_VITE_KEY_FILE=/k.pem", "", false)
	f.Add("[server]\nbase_url = \"https://blog.example.org/\"\ntrusted_proxies = [\"10.0.0.1\"]\n", "[server]\ntrusted_proxies = []\n",
		"PISATI_SERVER_TRUSTED_PROXIES=192.0.2.0/24, 2001:db8::/32\nPISATI_KEYS_MAIN_KEY_FILE=/run/k", "", true)
	f.Add("[keys]\nmain_key = \"x\"\n[nope]\n", "x = [", "PISATI_NOPE=1\nPISATI_LOG_LEVEL=loud", "relative", false)
	f.Add("server.base_url = 'https://[::1]:8443'\nserver.hsts = 'preload'", "[server]\nbase_url = \"http://127.0.0.1\"",
		"PISATI_SERVER_LISTEN=[::1]:1\nPISATI_TELEMETRY_OTLP_ENDPOINT=https://c/v1", "/", false)
	// The two inputs that found a path of one space, which no URL holds
	// unescaped, accepted and then spelt back as %20. Load refuses them
	// now for that, and for BAse_url, which is not server.base_url.
	f.Add("", "[server]\nBAse_url=\"http://127.0.0.1\"", "PISATI_TELEMETRY_OTLP_ENDPOINT=http://A/ ", "/", true)
	f.Add("", "[server]\nBAse_url=\"http://127.0.0.1\"", "PISATI_TELEMETRY_OTLP_ENDPOINT=http://A/ ", "/", false)
	// A table and a key in another case, which go-toml alone would match.
	f.Add("[SERVER]\nbase_url = \"https://blog.example.org\"\n", "[log]\nLEVEL = \"debug\"\n", "PISATI_SERVER_BASE_URL=https://b.example.org", "", false)
	// Files that end where a value is missing or left open, as the value
	// needled puts after them would be its own.
	for _, file := range []string{
		"[keys]\nmain_key =",
		"[keys]\r\nmain_key =\r\n",
		"[telemetry]\notlp_endpoint =\n  \"" + withUserInfo("https://", "someone-Uq7Zk", "hunter2-Pq7Zk", "collector.example.org") + "\"",
		"[telemetry]\notlp_endpoint =\nhttp://x",
		"[keys]\nmain_key = \"abc",
		"[keys]\n'abc\n'zz' = 1",
		"[keys]\nmain_key =\n\n\n",
	} {
		f.Add(file, "", "PISATI_SERVER_BASE_URL=https://b.example.org", "", false)
	}

	f.Fuzz(func(t *testing.T, file, dropIn, vars, stateDir string, credential bool) {
		e := env{}
		for line := range strings.Lines(vars) {
			name, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
			if name != "" {
				e[name] = value
			}
		}
		s := setup{
			files: fstest.MapFS{"pisati.toml": tomlFile(file), "pisati.toml.d/10-fuzz.toml": tomlFile(dropIn)},
			env:   e,
			state: stateDir,
		}
		if credential {
			s.creds = fstest.MapFS{"pisati-main-key": tomlFile("key")}
		}

		needled(t, file)
		cfg, err := s.load(t.Context())
		again, errAgain := s.load(t.Context())
		if fmt.Sprint(err) != fmt.Sprint(errAgain) || !reflect.DeepEqual(cfg, again) {
			t.Fatalf("Load is not deterministic: %v, %v", err, errAgain)
		}
		if err != nil {
			if !reflect.DeepEqual(cfg, config.Config{}) {
				t.Errorf("Load failed with %v but returned %+v", err, cfg)
			}
			errorsAreTheAPI(t, err)
			return
		}
		valid(t, cfg)
		for _, text := range []string{file, dropIn} {
			spelt(t, text)
		}
	})
}

// notDecoded is the fixed text of every error for a file that does not
// decode, after its file, line, column and key.
const notDecoded = "it is not TOML, or not a value the setting takes (what the file holds is not shown); " +
	"none of the file's settings are used until it decodes"

// needle is a value no fixed text holds by chance.
const needle = "Qz9vNEEDLEq7Zk"

// needled fails t when a decode error for file, followed by a line that
// holds needle quoted, holds needle but as the key it is on that line of
// its own. The line is the value a key the file ends with may miss, or
// one a string or an array left open at its end may hold.
func needled(t *testing.T, file string) {
	t.Helper()
	if strings.Contains(file, needle) {
		return
	}
	line := strings.Count(file, "\n") + 2
	_, err := setup{
		files: fstest.MapFS{"pisati.toml": tomlFile(file + "\n\"" + needle + "\"\n")},
		env:   env{"PISATI_SERVER_BASE_URL": baseURL},
	}.load(t.Context())
	if err == nil {
		return
	}
	for _, part := range errorParts(err) {
		msg := part.Error()
		var row, col int
		if _, scanErr := fmt.Sscanf(msg, "pisati.toml:%d:%d:", &row, &col); scanErr != nil || !strings.HasSuffix(msg, ": invalid: "+notDecoded) {
			continue
		}
		where := fmt.Sprintf("pisati.toml:%d:%d: ", row, col)
		key := strings.TrimSuffix(strings.TrimPrefix(msg, where), ": invalid: "+notDecoded)
		if row == line && (key == needle || strings.HasSuffix(key, "."+needle)) {
			continue // the line read as a key, which an error may name
		}
		if strings.Contains(msg, needle) {
			t.Errorf("error %q, for %q and the line after it, holds the value on that line", msg, file)
		}
	}
}

// spelt fails t unless every key in text, a file Load accepted, decoded
// by go-toml alone into a map, which keeps keys as they are written, is a
// section or a setting spelt as the settings table spells it.
func spelt(t *testing.T, text string) {
	t.Helper()
	var doc map[string]any
	if err := toml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("Load accepted %q, which go-toml does not decode: %v", text, err)
	}
	keys := make(map[string]bool)
	for _, r := range config.Table() {
		section, _, _ := strings.Cut(r.Key, ".")
		keys[section], keys[r.Key] = true, true
	}
	for section, v := range doc {
		table, ok := v.(map[string]any)
		if !keys[section] || !ok {
			t.Errorf("Load accepted %q, whose key %q is no section", text, section)
			continue
		}
		for name := range table {
			if !keys[section+"."+name] {
				t.Errorf("Load accepted %q, whose key %q is no setting", text, section+"."+name)
			}
		}
	}
}

// errorsAreTheAPI fails t unless each error err joins wraps one of the
// package's sentinels.
func errorsAreTheAPI(t *testing.T, err error) {
	t.Helper()
	parts := []error{err}
	if joined, ok := errors.AsType[interface {
		error
		Unwrap() []error
	}](err); ok {
		parts = joined.Unwrap()
	}
	for _, part := range parts {
		if !errors.Is(part, config.ErrInvalid) && !errors.Is(part, config.ErrUnknown) && !errors.Is(part, config.ErrMissing) {
			t.Errorf("error %q wraps none of ErrInvalid, ErrUnknown and ErrMissing", part)
		}
	}
}

func localhost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// valid fails t for every rule c breaks.
func valid(t *testing.T, c config.Config) {
	t.Helper()

	base := c.Server.BaseURL
	if again, err := config.ParseBaseURL(base.String()); err != nil || again != base {
		t.Errorf("base URL %q does not parse back to itself: %v, %v", base, again, err)
	}
	bu, err := url.Parse(base.String())
	if err != nil {
		t.Fatalf("base URL %q: %v", base, err)
	}
	if s := base.String(); strings.ContainsAny(s, "?#@") || bu.Path != "" || bu.Host == "" {
		t.Errorf("base URL %q has more than a scheme and a host", s)
	}
	if base.IsHTTPS() != (bu.Scheme == "https") || bu.Scheme != "https" && bu.Scheme != "http" {
		t.Errorf("base URL %q: IsHTTPS = %v", base, base.IsHTTPS())
	}
	if !base.IsHTTPS() && !localhost(bu.Hostname()) {
		t.Errorf("base URL %q is plain http away from localhost", base)
	}

	for _, a := range []config.ListenAddr{c.Server.Listen, c.Server.InternalListen} {
		if again, err := config.ParseListenAddr(a.String()); err != nil || again != a || a.Port() == 0 {
			t.Errorf("listen address %q does not parse back to itself: %v, %v", a, again, err)
		}
	}
	for _, p := range c.Server.TrustedProxies {
		if !p.IsValid() || p != p.Masked() {
			t.Errorf("trusted proxy %v is not a masked prefix", p)
		}
	}
	switch c.Server.HSTS {
	case config.HSTSHostOnly, config.HSTSIncludeSubdomains, config.HSTSPreload:
	default:
		t.Errorf("HSTS scope %d is not one of the constants", c.Server.HSTS)
	}
	if d := c.Storage.StateDir; !filepath.IsAbs(d) || filepath.Clean(d) != d {
		t.Errorf("state directory %q is not absolute and clean", d)
	}
	switch c.Log.Level {
	case slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError:
	default:
		t.Errorf("log level %v is not one of slog's four", c.Log.Level)
	}
	switch c.Log.Format {
	case config.LogJSON, config.LogText:
	default:
		t.Errorf("log format %d is not one of the constants", c.Log.Format)
	}
	switch c.Telemetry.Pprof {
	case config.Off, config.On:
	default:
		t.Errorf("pprof switch %d is not one of the constants", c.Telemetry.Pprof)
	}
	for _, o := range []config.OptionalURL{c.Telemetry.OTLPEndpoint, c.Dev.ViteURL} {
		u, ok := o.Get()
		if !ok {
			continue
		}
		again, err := config.ParseOptionalURL(u.String())
		if got, set := again.Get(); err != nil || !set || got != u {
			t.Errorf("URL %q does not parse back to itself: %v, %v", u.String(), got.String(), err)
		}
	}
	if u, ok := c.Dev.ViteURL.Get(); ok && (!localhost(u.Hostname()) || !localhost(bu.Hostname())) {
		t.Errorf("Vite URL %q with base URL %q: both must be on localhost", u.String(), base)
	}
	if u, ok := c.Dev.ViteURL.Get(); ok && u.Scheme == "https" && (c.Dev.ViteCertFile == "" || c.Dev.ViteKeyFile == "") {
		t.Errorf("https Vite URL %q with certificate %q and key %q: it needs both", u.String(), c.Dev.ViteCertFile, c.Dev.ViteKeyFile)
	}
	for _, p := range []string{c.Dev.ViteCertFile, c.Dev.ViteKeyFile} {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p) {
			t.Errorf("Vite certificate or key file %q is not absolute and clean", p)
		}
	}
	switch s := c.Keys.MainKey.(type) {
	case config.FromFile:
		if !filepath.IsAbs(s.Path) || filepath.Clean(s.Path) != s.Path {
			t.Errorf("main key file %q is not absolute and clean", s.Path)
		}
	case config.FromCredential:
		if s.Name != "pisati-main-key" {
			t.Errorf("main key credential %q", s.Name)
		}
	case config.Generated:
	case nil:
		t.Error("no main key source")
	}
}

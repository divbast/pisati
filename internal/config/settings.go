// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
)

// layer is what one source gives, still as text: each file decodes
// strictly into one, and the environment is read into another. A nil
// field is a setting the source leaves alone. Only the settings table
// reaches these fields, and a test holds their tags to its keys.
type layer struct {
	Server    serverLayer    `toml:"server"`
	Storage   storageLayer   `toml:"storage"`
	Log       logLayer       `toml:"log"`
	Telemetry telemetryLayer `toml:"telemetry"`
	Keys      keysLayer      `toml:"keys"`
	Dev       devLayer       `toml:"dev"`
}

type serverLayer struct {
	BaseURL        *string   `toml:"base_url"`
	Listen         *string   `toml:"listen"`
	InternalListen *string   `toml:"internal_listen"`
	TrustedProxies *[]string `toml:"trusted_proxies"`
	HSTS           *string   `toml:"hsts"`
}

type storageLayer struct {
	StateDir *string `toml:"state_dir"`
}

type logLayer struct {
	Level  *string `toml:"level"`
	Format *string `toml:"format"`
}

type telemetryLayer struct {
	OTLPEndpoint *string `toml:"otlp_endpoint"`
	Pprof        *string `toml:"pprof"`
}

type keysLayer struct {
	MainKey     *string `toml:"main_key"`
	MainKeyFile *string `toml:"main_key_file"`
}

type devLayer struct {
	ViteURL      *string `toml:"vite_url"`
	ViteCertFile *string `toml:"vite_cert_file"`
	ViteKeyFile  *string `toml:"vite_key_file"`
}

// setting is one row of the settings table.
type setting struct {
	// key is the TOML key, section.name. The variable is envName(key).
	key string
	// value says how a value is spelt.
	value string
	// absent is what build does when no source gives the setting, and
	// def the default, spelt as a source would give it, that it applies
	// when absent is applyDefault. A refused setting has neither.
	absent whenAbsent
	def    string
	kind   kind
}

// env returns the variable that sets s.
func (s setting) env() string {
	return envName(s.key)
}

// label names s in an error, by its TOML key and its variable.
func (s setting) label() string {
	return s.key + " (" + s.env() + ")"
}

// whenAbsent is what build does with a setting no source gives.
type whenAbsent uint8

const (
	// applyDefault applies the setting's default.
	applyDefault whenAbsent = iota
	// reportMissing reports the setting missing, as it has no default.
	// A source giving it empty counts as not giving it.
	reportMissing
	// leaveToRules leaves the setting's field to a rule Load applies
	// after the table.
	leaveToRules
)

// kind is what a setting's value is, and where it goes.
//
//sumtype:decl
type kind interface {
	isKind()
}

// text is a setting whose value is one string: field reaches it in a
// layer, and apply checks it and stores it in a Config.
type text struct {
	field func(*layer) **string
	apply func(*Config, string) error
}

// list is a setting whose value is a list of strings: an array in TOML,
// and comma-separated in a variable. apply returns one error for each
// item it refuses.
type list struct {
	field func(*layer) **[]string
	apply func(*Config, []string) []error
}

// refused is a setting known only to be refused: a secret given in
// place, where the setting instead should name the file that holds it.
// Its value is never read.
type refused struct {
	field   func(*layer) **string
	instead string
}

func (text) isKind()    {}
func (list) isKind()    {}
func (refused) isKind() {}

// settings is the settings table: every setting Pisati knows, in the
// order of the layer's fields. Decoding files, reading the environment,
// UnknownEnv and build all go by it. dev.vite_url comes after
// server.base_url, as its rule reads the base URL build has stored.
func settings() []setting {
	hsts := oneOf(HSTSScope.String, HSTSHostOnly, HSTSIncludeSubdomains, HSTSPreload)
	levels := oneOf(levelName, slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError)
	formats := oneOf(LogFormat.String, LogJSON, LogText)
	switches := oneOf(Switch.String, Off, On)
	absolutePath := "an absolute, clean path"
	return []setting{
		{
			key:    "server.base_url",
			value:  "https://host[:port], or http://host[:port] on localhost, 127.0.0.1 or [::1]",
			absent: reportMissing,
			kind: text{
				field: func(l *layer) **string { return &l.Server.BaseURL },
				apply: into(ParseBaseURL, func(c *Config) *BaseURL { return &c.Server.BaseURL }),
			},
		},
		{
			key:   "server.listen",
			value: "host:port, with a port from 1 to 65535; an empty host is every interface",
			def:   ":8080",
			kind: text{
				field: func(l *layer) **string { return &l.Server.Listen },
				apply: into(ParseListenAddr, func(c *Config) *ListenAddr { return &c.Server.Listen }),
			},
		},
		{
			key:   "server.internal_listen",
			value: "host:port, with a port from 1 to 65535; an empty host is every interface",
			def:   ":8081",
			kind: text{
				field: func(l *layer) **string { return &l.Server.InternalListen },
				apply: into(ParseListenAddr, func(c *Config) *ListenAddr { return &c.Server.InternalListen }),
			},
		},
		{
			key:   "server.trusted_proxies",
			value: "IP addresses and CIDR prefixes: an array in TOML, comma-separated in the variable",
			def:   "",
			kind: list{
				field: func(l *layer) **[]string { return &l.Server.TrustedProxies },
				apply: applyTrustedProxies,
			},
		},
		{
			key:   "server.hsts",
			value: hsts.spelling(),
			def:   HSTSHostOnly.String(),
			kind: text{
				field: func(l *layer) **string { return &l.Server.HSTS },
				apply: into(hsts.parse, func(c *Config) *HSTSScope { return &c.Server.HSTS }),
			},
		},
		{
			key:   "storage.state_dir",
			value: absolutePath,
			def:   defaultStateDir,
			kind: text{
				field: func(l *layer) **string { return &l.Storage.StateDir },
				apply: into(parsePath, func(c *Config) *string { return &c.Storage.StateDir }),
			},
		},
		{
			key:   "log.level",
			value: levels.spelling(),
			def:   levelName(slog.LevelInfo),
			kind: text{
				field: func(l *layer) **string { return &l.Log.Level },
				apply: into(levels.parse, func(c *Config) *slog.Level { return &c.Log.Level }),
			},
		},
		{
			key:   "log.format",
			value: formats.spelling(),
			def:   LogJSON.String(),
			kind: text{
				field: func(l *layer) **string { return &l.Log.Format },
				apply: into(formats.parse, func(c *Config) *LogFormat { return &c.Log.Format }),
			},
		},
		{
			key:   "telemetry.otlp_endpoint",
			value: "an http or https URL, or empty for none",
			def:   "",
			kind: text{
				field: func(l *layer) **string { return &l.Telemetry.OTLPEndpoint },
				apply: into(ParseOptionalURL, func(c *Config) *OptionalURL { return &c.Telemetry.OTLPEndpoint }),
			},
		},
		{
			key:   "telemetry.pprof",
			value: switches.spelling(),
			def:   Off.String(),
			kind: text{
				field: func(l *layer) **string { return &l.Telemetry.Pprof },
				apply: into(switches.parse, func(c *Config) *Switch { return &c.Telemetry.Pprof }),
			},
		},
		{
			key:   "keys.main_key",
			value: "never accepted: name the file that holds the key with keys.main_key_file",
			kind: refused{
				field:   func(l *layer) **string { return &l.Keys.MainKey },
				instead: "keys.main_key_file",
			},
		},
		{
			// With none, Load looks for the credential, then falls back
			// to Generated.
			key:    "keys.main_key_file",
			value:  absolutePath,
			absent: leaveToRules,
			kind: text{
				field: func(l *layer) **string { return &l.Keys.MainKeyFile },
				apply: into(fileSource, func(c *Config) *SecretSource { return &c.Keys.MainKey }),
			},
		},
		{
			key:   "dev.vite_url",
			value: "an http or https URL on localhost, 127.0.0.1 or [::1], with a base URL there too; or empty for none",
			def:   "",
			kind: text{
				field: func(l *layer) **string { return &l.Dev.ViteURL },
				apply: applyViteURL,
			},
		},
		{
			// Required when dev.vite_url is https, and used only then.
			key:    "dev.vite_cert_file",
			value:  absolutePath + ", used only with an https dev.vite_url",
			absent: leaveToRules,
			kind: text{
				field: func(l *layer) **string { return &l.Dev.ViteCertFile },
				apply: into(filePath, func(c *Config) *string { return &c.Dev.ViteCertFile }),
			},
		},
		{
			key:    "dev.vite_key_file",
			value:  absolutePath + ", used only with an https dev.vite_url",
			absent: leaveToRules,
			kind: text{
				field: func(l *layer) **string { return &l.Dev.ViteKeyFile },
				apply: into(filePath, func(c *Config) *string { return &c.Dev.ViteKeyFile }),
			},
		},
	}
}

// into returns an apply that parses a value with parse and stores what it
// gives in the field of the Config that field reaches.
func into[T any](parse func(string) (T, error), field func(*Config) *T) func(*Config, string) error {
	return func(c *Config, s string) error {
		v, err := parse(s)
		if err != nil {
			return err
		}
		*field(c) = v
		return nil
	}
}

// applyTrustedProxies stores items, each a bare address or a CIDR prefix
// with no host bits set, as the trusted proxies.
func applyTrustedProxies(c *Config, items []string) []error {
	var errs []error
	c.Server.TrustedProxies = make([]netip.Prefix, 0, len(items))
	for _, item := range items {
		p, err := parsePrefix(item)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.Server.TrustedProxies = append(c.Server.TrustedProxies, p)
	}
	return errs
}

// applyViteURL stores s as the Vite dev server's URL. A URL is accepted
// only on localhost, 127.0.0.1 or [::1], with a base URL there too; c
// already holds the base URL, unless it was refused.
func applyViteURL(c *Config, s string) error {
	vite, err := ParseOptionalURL(s)
	if err != nil {
		return err
	}
	u, set := vite.Get()
	baseGiven := c.Server.BaseURL != BaseURL{}
	if set && baseGiven && (!c.Server.BaseURL.local || !loopback(u.Hostname())) {
		return fmt.Errorf("%w value %q: the Vite dev server is for development alone, so it and the base URL must both be on localhost, 127.0.0.1 or [::1]", ErrInvalid, u.String())
	}
	c.Dev.ViteURL = vite
	return nil
}

// fileSource returns the file s names as a secret's source. Its error
// never repeats s, which may be the secret itself, given by mistake in
// place of its file's path.
func fileSource(s string) (SecretSource, error) {
	p, err := filePath(s)
	if err != nil {
		return nil, err
	}
	return FromFile{Path: p}, nil
}

// filePath returns s, the path of a file that holds a secret or goes with
// one, when it is absolute and clean. Its error never repeats s, which may
// be the secret itself, given by mistake in place of its file's path.
func filePath(s string) (string, error) {
	if !cleanAbsolute(s) {
		return "", fmt.Errorf("%w value: want an absolute, clean path (the value is not shown, as it may be the secret itself)", ErrInvalid)
	}
	return s, nil
}

// parsePath returns s when it is an absolute, clean path.
func parsePath(s string) (string, error) {
	if !cleanAbsolute(s) {
		return "", fmt.Errorf("%w value %q: want an absolute, clean path", ErrInvalid, s)
	}
	return s, nil
}

// cleanAbsolute reports whether s is an absolute path that filepath.Clean
// leaves as it is.
func cleanAbsolute(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s
}

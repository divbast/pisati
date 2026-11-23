// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"slices"
	"strings"
)

// envPrefix starts every variable Pisati reads.
const envPrefix = "PISATI_"

// layer is what one source gives, still as text: each file decodes
// strictly into one, and the environment is read into another. A nil
// field is a setting the source leaves alone.
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
	// MasterKey is known only to be refused: a secret is never written
	// into the configuration itself.
	MasterKey     *string `toml:"master_key"`
	MasterKeyFile *string `toml:"master_key_file"`
}

type devLayer struct {
	ViteURL *string `toml:"vite_url"`
}

// setting is one known setting: its TOML key, section.name, and the field
// of a layer that holds it.
type setting struct {
	key  string
	slot slot
}

// slot reaches a setting's field in a layer.
//
//sumtype:decl
type slot interface {
	isSlot()
}

// textSlot reaches a setting whose value is one string.
type textSlot func(*layer) **string

// listSlot reaches a setting whose value is a list of strings: an array
// in TOML, and comma-separated in a variable.
type listSlot func(*layer) **[]string

func (textSlot) isSlot() {}
func (listSlot) isSlot() {}

// settings is every setting Pisati knows: merging files, reading the
// environment and UnknownEnv all go by it. Each one's TOML key matches
// its layer field's tags, and its variable is envName of the key.
func settings() []setting {
	return []setting{
		{key: "server.base_url", slot: textSlot(func(l *layer) **string { return &l.Server.BaseURL })},
		{key: "server.listen", slot: textSlot(func(l *layer) **string { return &l.Server.Listen })},
		{key: "server.internal_listen", slot: textSlot(func(l *layer) **string { return &l.Server.InternalListen })},
		{key: "server.trusted_proxies", slot: listSlot(func(l *layer) **[]string { return &l.Server.TrustedProxies })},
		{key: "server.hsts", slot: textSlot(func(l *layer) **string { return &l.Server.HSTS })},
		{key: "storage.state_dir", slot: textSlot(func(l *layer) **string { return &l.Storage.StateDir })},
		{key: "log.level", slot: textSlot(func(l *layer) **string { return &l.Log.Level })},
		{key: "log.format", slot: textSlot(func(l *layer) **string { return &l.Log.Format })},
		{key: "telemetry.otlp_endpoint", slot: textSlot(func(l *layer) **string { return &l.Telemetry.OTLPEndpoint })},
		{key: "telemetry.pprof", slot: textSlot(func(l *layer) **string { return &l.Telemetry.Pprof })},
		{key: "keys.master_key", slot: textSlot(func(l *layer) **string { return &l.Keys.MasterKey })},
		{key: "keys.master_key_file", slot: textSlot(func(l *layer) **string { return &l.Keys.MasterKeyFile })},
		{key: "dev.vite_url", slot: textSlot(func(l *layer) **string { return &l.Dev.ViteURL })},
	}
}

// envName returns the variable that sets the setting key:
// PISATI_<SECTION>_<NAME>, in upper case.
func envName(key string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// label names the setting key in an error, by its TOML key and its
// variable.
func label(key string) string {
	return key + " (" + envName(key) + ")"
}

// overlay sets in dst each setting src sets.
func overlay(dst, src *layer) {
	for _, s := range settings() {
		switch slot := s.slot.(type) {
		case textSlot:
			if v := *slot(src); v != nil {
				*slot(dst) = v
			}
		case listSlot:
			if v := *slot(src); v != nil {
				*slot(dst) = v
			}
		}
	}
}

// fromEnv returns the layer the variables lookup finds give. A nil lookup
// finds none.
func fromEnv(lookup func(string) (string, bool)) layer {
	var l layer
	if lookup == nil {
		return l
	}
	for _, s := range settings() {
		v, ok := lookup(envName(s.key))
		if !ok {
			continue
		}
		switch slot := s.slot.(type) {
		case textSlot:
			*slot(&l) = &v
		case listSlot:
			items := splitList(v)
			*slot(&l) = &items
		}
	}
	return l
}

// splitList splits a variable's comma-separated list, trimming the space
// around each item. A blank value is the empty list.
func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return []string{}
	}
	items := strings.Split(v, ",")
	for i, item := range items {
		items[i] = strings.TrimSpace(item)
	}
	return items
}

// UnknownEnv returns the names in names that look like Pisati's, starting
// with PISATI_ in any case, but that name no setting: sorted, each once,
// and nil when there are none. It leaves names as it was.
func UnknownEnv(names []string) []string {
	known := make(map[string]struct{})
	for _, s := range settings() {
		known[envName(s.key)] = struct{}{}
	}
	var unknown []string
	for _, name := range names {
		if _, ok := known[name]; ok || len(name) < len(envPrefix) || !strings.EqualFold(name[:len(envPrefix)], envPrefix) {
			continue
		}
		unknown = append(unknown, name)
	}
	slices.Sort(unknown)
	return slices.Compact(unknown)
}

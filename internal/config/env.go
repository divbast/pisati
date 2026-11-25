// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"slices"
	"strconv"
	"strings"
)

// envPrefix starts every variable Pisati reads.
const envPrefix = "PISATI_"

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
		switch k := s.kind.(type) {
		case text:
			if v := *k.field(src); v != nil {
				*k.field(dst) = v
			}
		case list:
			if v := *k.field(src); v != nil {
				*k.field(dst) = v
			}
		case refused:
			if v := *k.field(src); v != nil {
				*k.field(dst) = v
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
		v, ok := lookup(s.env())
		if !ok {
			continue
		}
		switch k := s.kind.(type) {
		case text:
			*k.field(&l) = &v
		case list:
			items := splitList(v)
			*k.field(&l) = &items
		case refused:
			*k.field(&l) = &v
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
//
// It leaves out the variables Kubernetes sets for a Service named pisati,
// in every container of the Service's namespace, Pisati's among them:
// PISATI_SERVICE_HOST, PISATI_SERVICE_PORT and PISATI_SERVICE_PORT_<name>,
// for each named port, PISATI_PORT, and PISATI_PORT_<port>_<protocol>,
// alone or followed by _PROTO, _PORT or _ADDR, for each port, where
// <protocol> is TCP, UDP or SCTP. No setting's variable takes one of these
// shapes, so a misspelt one is still returned.
//
// Load already applies it to Sources.EnvNames and reports each name it
// returns as ErrUnknown, so a caller of Load need not call it as well.
func UnknownEnv(names []string) []string {
	known := make(map[string]struct{})
	for _, s := range settings() {
		known[s.env()] = struct{}{}
	}
	var unknown []string
	for _, name := range names {
		if _, ok := known[name]; ok || len(name) < len(envPrefix) || !strings.EqualFold(name[:len(envPrefix)], envPrefix) ||
			serviceLink(name) {
			continue
		}
		unknown = append(unknown, name)
	}
	slices.Sort(unknown)
	return slices.Compact(unknown)
}

// serviceLink reports whether name is one of the variables Kubernetes
// sets for a Service named pisati, as UnknownEnv lists them. Kubernetes
// writes them in capital letters, with a port's name in capitals and its
// hyphens as underscores, and a port's number in decimal.
func serviceLink(name string) bool {
	rest, ok := strings.CutPrefix(name, envPrefix)
	if !ok {
		return false
	}
	switch rest {
	case "SERVICE_HOST", "SERVICE_PORT", "PORT":
		return true
	}
	if portName, ok := strings.CutPrefix(rest, "SERVICE_PORT_"); ok {
		return portName != "" && !strings.ContainsFunc(portName, func(r rune) bool {
			return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_'
		})
	}
	link, ok := strings.CutPrefix(rest, "PORT_")
	if !ok {
		return false
	}
	number, protocol, ok := strings.Cut(link, "_")
	if !ok || !portNumber(number) {
		return false
	}
	for _, p := range []string{"TCP", "UDP", "SCTP"} {
		if part, ok := strings.CutPrefix(protocol, p); ok {
			return part == "" || part == "_PROTO" || part == "_PORT" || part == "_ADDR"
		}
	}
	return false
}

// portNumber reports whether s is a port, from 1 to 65535, in decimal as
// Kubernetes writes it, with no leading zero.
func portNumber(s string) bool {
	n, err := strconv.ParseUint(s, 10, 16)
	return err == nil && n > 0 && !strings.HasPrefix(s, "0")
}

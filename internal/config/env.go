// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"net"
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
// It leaves out the variables Kubernetes sets, in every container of a
// namespace, for each Service there whose name starts with pisati, Pisati's
// own among them. For a Service named pisati-<name>, with <name> written as
// Kubernetes writes it, in capitals and with "_" for "-", and for one named
// pisati itself, with no <name>_ at all, they are:
//   - PISATI_<name>_SERVICE_HOST and PISATI_<name>_SERVICE_PORT;
//   - PISATI_<name>_SERVICE_PORT_<port name>, for each named port;
//   - PISATI_<name>_PORT_<port>_<protocol>, alone or followed by _PROTO,
//     _PORT or _ADDR, for each port, where <protocol> is TCP, UDP or SCTP;
//   - PISATI_<name>_PORT, but only when lookup finds it set to the value
//     the kubelet writes there, <protocol>://<address>:<port>, with the
//     protocol in small letters: a typo such as PISATI_SERVER_PORT=8080 is
//     still returned. A nil lookup finds none.
//
// No setting's variable takes one of these shapes, so a misspelt one is
// still returned.
//
// Load already applies it to Sources.EnvNames and Sources.Env, and reports
// each name it returns as ErrUnknown, so a caller of Load need not call it
// as well.
func UnknownEnv(names []string, lookup func(string) (string, bool)) []string {
	known := make(map[string]struct{})
	for _, s := range settings() {
		known[s.env()] = struct{}{}
	}
	var unknown []string
	for _, name := range names {
		if _, ok := known[name]; ok || len(name) < len(envPrefix) || !strings.EqualFold(name[:len(envPrefix)], envPrefix) ||
			serviceLink(name, lookup) {
			continue
		}
		unknown = append(unknown, name)
	}
	slices.Sort(unknown)
	return slices.Compact(unknown)
}

// serviceLink reports whether name is one of the variables Kubernetes
// sets for a Service whose name starts with pisati, as UnknownEnv lists
// them, looking a bare …_PORT's value up with lookup.
func serviceLink(name string, lookup func(string) (string, bool)) bool {
	rest, ok := strings.CutPrefix(name, envPrefix)
	if !ok {
		return false
	}
	link := func(tail string) bool {
		switch tail {
		case "SERVICE_HOST", "SERVICE_PORT":
			return true
		case "PORT":
			if lookup == nil {
				return false
			}
			v, ok := lookup(name)
			return ok && linkAddress(v)
		}
		if portName, ok := strings.CutPrefix(tail, "SERVICE_PORT_"); ok {
			return portName != "" && !strings.ContainsFunc(portName, notLinkRune)
		}
		if port, ok := strings.CutPrefix(tail, "PORT_"); ok {
			return portLink(port)
		}
		return false
	}
	// The Service named pisati itself, then each place a name pisati-<name>
	// could end: the variable is a link when what follows is one.
	if link(rest) {
		return true
	}
	for i := range len(rest) {
		if rest[i] == '_' && serviceName(rest[:i]) && link(rest[i+1:]) {
			return true
		}
	}
	return false
}

// serviceName reports whether s is the part of a Service's name after
// "pisati-", as Kubernetes writes it in a variable's name: capitals,
// digits and "_", starting and ending with no "_", as a DNS label starts
// and ends with no "-".
func serviceName(s string) bool {
	return s != "" && s[0] != '_' && s[len(s)-1] != '_' && !strings.ContainsFunc(s, notLinkRune)
}

// notLinkRune reports whether r is none of what Kubernetes writes in a
// Service's or a port's name: a capital letter, a digit or "_".
func notLinkRune(r rune) bool {
	return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_'
}

// portLink reports whether s, what follows PORT_ in a variable's name, is
// <port>_<protocol>, alone or followed by _PROTO, _PORT or _ADDR.
func portLink(s string) bool {
	number, protocol, ok := strings.Cut(s, "_")
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

// linkAddress reports whether v is the value the kubelet writes in a bare
// …_PORT variable: <protocol>://<address>:<port>, with tcp, udp or sctp as
// the protocol, an address, and a port from 1 to 65535.
func linkAddress(v string) bool {
	for _, scheme := range []string{"tcp://", "udp://", "sctp://"} {
		if hostPort, ok := strings.CutPrefix(v, scheme); ok {
			host, port, err := net.SplitHostPort(hostPort)
			return err == nil && host != "" && portNumber(port)
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

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"reflect"
	"strings"
)

// Row is one row of the settings table, as the tests read it.
type Row struct {
	Key, Env, Value string
	// Absent is "default", "missing", "rules" or "refused": what happens
	// when no source gives the setting, or that it is always refused.
	Absent string
	// Default is the default, spelt as a source would give it, when
	// Absent is "default".
	Default string
}

// Table returns the settings table.
func Table() []Row {
	var rows []Row
	for _, s := range settings() {
		r := Row{Key: s.key, Env: s.env(), Value: s.value, Default: s.def}
		switch s.kind.(type) {
		case refused:
			r.Absent = "refused"
		case text, list:
			switch s.absent {
			case applyDefault:
				r.Absent = "default"
			case reportMissing:
				r.Absent = "missing"
			case leaveToRules:
				r.Absent = "rules"
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// LayerKeys returns the TOML key, section.name, of each field of layer, in
// their order, as their tags give it.
func LayerKeys() []string {
	return tagKeys(reflect.ValueOf(layer{}), func(reflect.Value) bool { return true })
}

// SlotKeys returns, for each row of the table, the TOML key of the layer
// field its kind reaches, by setting that field alone and reading which
// field is set.
func SlotKeys() []string {
	var keys []string
	for _, s := range settings() {
		var l layer
		switch k := s.kind.(type) {
		case text:
			v := ""
			*k.field(&l) = &v
		case list:
			v := []string{}
			*k.field(&l) = &v
		case refused:
			v := ""
			*k.field(&l) = &v
		}
		set := tagKeys(reflect.ValueOf(l), func(f reflect.Value) bool { return !f.IsNil() })
		keys = append(keys, strings.Join(set, "+"))
	}
	return keys
}

// tagKeys returns section.name, from the toml tags, of each field of each
// section of l that keep accepts.
func tagKeys(l reflect.Value, keep func(reflect.Value) bool) []string {
	var keys []string
	for i := range l.NumField() {
		section := l.Field(i)
		sectionName := l.Type().Field(i).Tag.Get("toml")
		for j := range section.NumField() {
			if keep(section.Field(j)) {
				keys = append(keys, sectionName+"."+section.Type().Field(j).Tag.Get("toml"))
			}
		}
	}
	return keys
}

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// inOtherCase reports whether doc, a file decoded into a map, which keeps
// keys as written, holds a table or a key that go-toml would match to a
// section or a setting though it is spelt otherwise: in another case,
// which strings.ToLower, as go-toml lowers a key to match it, makes the
// settings table's.
func inOtherCase(doc map[string]any) bool {
	for name, v := range doc {
		section := strings.ToLower(name)
		if !knownSection(section) {
			continue
		}
		if name != section {
			return true
		}
		table, ok := v.(map[string]any)
		if !ok {
			continue // a section that holds no table holds no key either
		}
		for key := range table {
			if lower := strings.ToLower(key); lower != key && knownSetting(section+"."+lower) {
				return true
			}
		}
	}
	return false
}

// fromMap returns the layer doc, the file name decoded into a map, gives:
// each section and setting spelt exactly as the settings table spells it.
// It returns an error wrapping ErrUnknown for each table or key spelt any
// other way, named as written with the file, a table once for all it
// holds; an error wrapping ErrInvalid for each value not of its setting's
// type; and the keys of the settings that a key in another case would
// set, which, since that key is reported, are not to be reported missing.
func fromMap(name string, doc map[string]any) (l layer, unknown, wrongType []error, otherCase map[string]bool) {
	otherCase = make(map[string]bool)
	for _, tableName := range slices.Sorted(maps.Keys(doc)) {
		table, isTable := doc[tableName].(map[string]any)
		section := strings.ToLower(tableName)
		if tableName != section || !knownSection(section) {
			unknown = append(unknown, fmt.Errorf("%s: %s: %w", name, tableName, ErrUnknown))
			if knownSection(section) {
				for key := range table {
					if s := section + "." + strings.ToLower(key); knownSetting(s) {
						otherCase[s] = true
					}
				}
			}
			continue
		}
		if !isTable {
			wrongType = append(wrongType, fmt.Errorf("%s: %s: %w: %s", name, tableName, ErrInvalid, notDecoded))
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(table)) {
			if knownSetting(section + "." + key) {
				continue
			}
			unknown = append(unknown, fmt.Errorf("%s: %s.%s: %w", name, tableName, key, ErrUnknown))
			if s := section + "." + strings.ToLower(key); knownSetting(s) {
				otherCase[s] = true
			}
		}
	}
	for _, s := range settings() {
		section, key, _ := strings.Cut(s.key, ".")
		// A section that holds no table, refused above, gives no setting.
		table, ok := doc[section].(map[string]any)
		v, given := table[key]
		if !ok || !given {
			continue
		}
		if !fromValue(&l, s, v) {
			wrongType = append(wrongType, fmt.Errorf("%s: %s: %w: %s", name, s.key, ErrInvalid, notDecoded))
		}
	}
	return l, unknown, wrongType, otherCase
}

// fromValue sets s in l to v, a value go-toml decoded into a map, and
// reports whether v is of s's type: a string, or for a list an array of
// strings. A refused setting takes any value, which it never reads.
func fromValue(l *layer, s setting, v any) bool {
	switch k := s.kind.(type) {
	case text:
		str, ok := v.(string)
		if ok {
			*k.field(l) = &str
		}
		return ok
	case list:
		values, ok := v.([]any)
		items := make([]string, 0, len(values))
		for _, value := range values {
			item, isString := value.(string)
			ok = ok && isString
			items = append(items, item)
		}
		if ok {
			*k.field(l) = &items
		}
		return ok
	case refused:
		given := ""
		*k.field(l) = &given
		return true
	}
	return false
}

// knownSection reports whether name is a section the settings table
// spells, exactly.
func knownSection(name string) bool {
	return slices.ContainsFunc(settings(), func(s setting) bool {
		section, _, _ := strings.Cut(s.key, ".")
		return section == name
	})
}

// knownSetting reports whether key, section.name, is a setting the
// settings table spells, exactly.
func knownSetting(key string) bool {
	return slices.ContainsFunc(settings(), func(s setting) bool { return s.key == key })
}

// failedKey returns the key of the expression data fails to decode in, as
// TOML, at offset at, with the table that holds it: as much of the key as
// reads, or the table alone when no key does or the failure is in a
// comment. go-toml's errors name no key for text that does not parse, and
// for a key given twice only the key as its table holds it.
//
// Every expression before the one that fails is valid TOML, so a scan that
// follows only lines, strings, brackets and comments finds where each
// starts, and so where the failing one does and the table it is in.
func failedKey(data []byte, at int) []string {
	start, header := expressionAt(data, at)
	var table []string
	if header >= 0 {
		table = headerKey(data[header:])
	}
	rest := data[start:]
	switch {
	case len(rest) == 0 || rest[0] == '#':
		return table
	case rest[0] == '[':
		return headerKey(rest)
	}
	return append(table, keyParts(rest)...)
}

// expressionAt returns where the expression of data that holds offset at
// starts, a comment counting as one, and where the last table header
// before it starts, or -1 when there is none. An expression holds the
// line ending that ends it: go-toml reports there a value that is missing,
// a string left open and a key with no "=", and the text after it is the
// next expression's, never this one's key.
func expressionAt(data []byte, at int) (start, header int) {
	header = -1
	i := 0
	for {
		i += betweenExpressions(data[i:])
		if i >= at || i >= len(data) {
			return i, header
		}
		end := expressionEnd(data, i)
		if at < end {
			return i, header
		}
		if data[i] == '[' {
			header = i
		}
		i = end
	}
}

// betweenExpressions returns how many bytes b starts with that lie between
// expressions: spaces, tabs and line endings, LF or CRLF. A carriage return
// alone is no line ending, and go-toml fails there: it starts no
// expression's key.
func betweenExpressions(b []byte) int {
	n := 0
	for n < len(b) {
		switch {
		case b[n] == ' ' || b[n] == '\t' || b[n] == '\n':
			n++
		case bytes.HasPrefix(b[n:], []byte("\r\n")):
			n += 2
		default:
			return n
		}
	}
	return n
}

// expressionEnd returns the offset just past the line ending that ends
// the expression starting at offset i of data, or the length of data: the
// first one outside a string and outside brackets and braces, which a
// table header and a key-value's arrays and inline tables open, comments
// skipped.
func expressionEnd(data []byte, i int) int {
	depth := 0
	for i < len(data) {
		switch c := data[i]; {
		case c == '\n' && depth == 0:
			return i + 1
		case c == '#':
			i = lineEnd(data, i)
			continue
		case c == '"' || c == '\'':
			i = stringEnd(data, i)
			continue
		case c == '[' || c == '{':
			depth++
		case (c == ']' || c == '}') && depth > 0:
			depth--
		}
		i++
	}
	return len(data)
}

// stringEnd returns the offset just past the string that starts at offset
// i of data, basic or literal, on one line or many; or where the text
// ends, or the line does for a string on one line, when it is not closed.
func stringEnd(data []byte, i int) int {
	q := data[i]
	delim := []byte{q, q, q}
	if bytes.HasPrefix(data[i:], delim) {
		for j := i + 3; j < len(data); j++ {
			switch {
			case q == '"' && data[j] == '\\':
				j++ // the escaped character, which cannot end the string
			case bytes.HasPrefix(data[j:], delim):
				// Up to two more quotes are the string's, before its end.
				end := j + 3
				for n := 0; n < 2 && end < len(data) && data[end] == q; n++ {
					end++
				}
				return end
			}
		}
		return len(data)
	}
	for j := i + 1; j < len(data) && data[j] != '\n'; j++ {
		switch {
		case q == '"' && data[j] == '\\':
			j++
		case data[j] == q:
			return j + 1
		}
	}
	return lineEnd(data, i)
}

// headerKey returns the parts of the key of the table header b starts
// with, as far as it reads.
func headerKey(b []byte) []string {
	b = bytes.TrimPrefix(bytes.TrimPrefix(b, []byte("[")), []byte("["))
	return keyParts(b[blanks(b):])
}

// lineEnd returns the offset of the newline that ends the line holding
// offset i of data, or the length of data on its last line.
func lineEnd(data []byte, i int) int {
	if n := bytes.IndexByte(data[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(data)
}

// keyParts returns the parts of the key b starts with, as far as it reads
// as one, or nil when it starts with none: a quoted part without its
// quotes and escapes, as go-toml reads it.
func keyParts(b []byte) []string {
	n := keyLength(b)
	if n == 0 {
		return nil
	}
	// The key alone, made a whole document for go-toml to decode, as
	// nested tables down to the one value.
	var doc map[string]any
	if err := toml.Unmarshal(append(slices.Clone(b[:n]), " = 0"...), &doc); err != nil {
		return nil
	}
	var parts []string
	for table, ok := doc, true; ok && len(table) == 1; {
		for name, v := range table {
			parts = append(parts, name)
			table, ok = v.(map[string]any)
		}
	}
	return parts
}

// keyLength returns the length of the dotted key b starts with, as TOML
// spells one: bare parts of letters, digits, "-" and "_", or quoted ones,
// joined by dots with white space around them; 0 when b starts with none.
func keyLength(b []byte) int {
	end, n := 0, 0
	for {
		part := simpleKeyLength(b[n:])
		if part == 0 {
			return end
		}
		end = n + part
		n = end + blanks(b[end:])
		if n >= len(b) || b[n] != '.' {
			return end
		}
		n++
		n += blanks(b[n:])
	}
}

// blanks returns how many spaces and tabs b starts with.
func blanks(b []byte) int {
	return len(b) - len(bytes.TrimLeft(b, " \t"))
}

// simpleKeyLength returns the length of the one part of a key b starts
// with, bare or quoted, or 0 when it starts with none.
func simpleKeyLength(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	if q := b[0]; q == '"' || q == '\'' {
		for i := 1; i < len(b) && b[i] != '\n'; i++ {
			switch {
			case b[i] == q:
				return i + 1
			case q == '"' && b[i] == '\\':
				i++ // the escaped character, which cannot end the part
			}
		}
		return 0
	}
	n := 0
	for n < len(b) && bareKeyByte(b[n]) {
		n++
	}
	return n
}

// bareKeyByte reports whether c may appear in a bare key.
func bareKeyByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}

// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"bytes"
	"slices"

	"github.com/pelletier/go-toml/v2"
)

// failedKey returns the key of the expression data fails to parse in, as
// TOML, at offset at, with the table that holds it: as much of the key as
// reads, or the table alone when no key does or the failure is in a
// comment. go-toml's errors name the key for a value it cannot decode, but
// none for text that does not parse.
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
// before it starts, or -1 when there is none.
func expressionAt(data []byte, at int) (start, header int) {
	header = -1
	i := 0
	for {
		for i < len(data) && bytes.IndexByte([]byte(" \t\r\n"), data[i]) >= 0 {
			i++
		}
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

// expressionEnd returns the offset of the line ending that ends the
// expression starting at offset i of data, or the length of data: the
// first one outside a string and outside brackets and braces, which a
// table header and a key-value's arrays and inline tables open, comments
// skipped.
func expressionEnd(data []byte, i int) int {
	depth := 0
	for i < len(data) {
		switch c := data[i]; {
		case c == '\n' && depth == 0:
			return i
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

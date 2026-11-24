// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package config

import (
	"bytes"
	"slices"

	"github.com/pelletier/go-toml/v2/unstable"
)

// failedKey returns the key of the expression data fails to parse in, as
// TOML, at offset at, with the table that holds it: as much of the key as
// reads, or the table alone when no key does or the failure is in a
// comment. It returns nil when data parses, or when nothing names the
// place. go-toml's errors name the key for a value it cannot decode, but
// none for text that does not parse.
func failedKey(data []byte, at int) []string {
	var p unstable.Parser
	p.Reset(data)
	var table []string
	end := 0
	for p.NextExpression() {
		// Each expression ends its line, but for a comment: a table
		// header after its key, a key-value after its value.
		e := p.Expression()
		if header(e.Kind) {
			table = keyOf(e)
			end = lineEnd(data, keyEnd(e))
		} else {
			end = lineEnd(data, int(e.Raw.Offset+e.Raw.Length))
		}
	}
	if p.Error() == nil {
		return nil
	}
	rest := data[expressionStart(data, end, at):]
	if h, ok := bytes.CutPrefix(rest, []byte("[")); ok {
		h = bytes.TrimPrefix(h, []byte("["))
		return keyParts(h[blanks(h):])
	}
	return append(slices.Clone(table), keyParts(rest)...)
}

// header reports whether an expression of kind k is a table header.
func header(k unstable.Kind) bool {
	return k == unstable.Table || k == unstable.ArrayTable
}

// expressionStart returns where the expression that fails at offset at
// starts: past the white space, line endings and comments from offset
// from on, but not past at. A failure in a comment so ends the comment's
// line, where no key starts.
func expressionStart(data []byte, from, at int) int {
	i := from
	for i < at && i < len(data) {
		switch data[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '#':
			i = lineEnd(data, i)
		default:
			return i
		}
	}
	return i
}

// keyOf returns the parts of the key of e, a table header or a key-value,
// as TOML reads them: a quoted part without its quotes and escapes.
func keyOf(e *unstable.Node) []string {
	var parts []string
	for it := e.Key(); it.Next(); {
		parts = append(parts, string(it.Node().Data))
	}
	return parts
}

// keyEnd returns the offset just past the last part of e's key.
func keyEnd(e *unstable.Node) int {
	end := 0
	for it := e.Key(); it.Next(); {
		end = int(it.Node().Raw.Offset + it.Node().Raw.Length)
	}
	return end
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
// as one, or nil when it starts with none.
func keyParts(b []byte) []string {
	n := keyLength(b)
	if n == 0 {
		return nil
	}
	// The key alone, made a whole expression for the parser to read.
	var p unstable.Parser
	p.Reset(append(slices.Clone(b[:n]), " = 0"...))
	if !p.NextExpression() || p.Expression().Kind != unstable.KeyValue {
		return nil
	}
	return keyOf(p.Expression())
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

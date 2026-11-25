// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The bounds on Params that HashPassword and ParseHash accept, so that a
// stored hash cannot make Verify take unbounded memory or time.
const (
	// maxMemory is 1 GiB, in KiB.
	maxMemory = 1 << 20
	// minMemoryPerThread is RFC 9106's least memory for each thread, in KiB.
	minMemoryPerThread = 8
	maxTime            = 16
	keyLen             = 32
)

// Params are argon2id's cost parameters. HashPassword and ParseHash accept
// Time from 1 to 16 passes, Threads from 1 to 255, and Memory from 8 KiB
// for each thread to 1 GiB, and refuse others; the zero Params is refused.
type Params struct {
	// Memory is in KiB.
	Memory, Time uint32
	Threads      uint8
}

// DefaultParams returns the params Pisati hashes with: RFC 9106's second
// recommended setting, 64 MiB, 3 passes and 4 threads. A hash made with
// any others needs rehashing (see Match).
func DefaultParams() Params {
	return Params{Memory: 64 * 1024, Time: 3, Threads: 4}
}

// check returns an error wrapping ErrInvalid unless p is within the
// bounds Params documents.
func (p Params) check() error {
	switch {
	case p.Time < 1 || p.Time > maxTime:
		return refuse("the password hash's settings ask for %d passes, but Pisati takes 1 to %d", p.Time, maxTime)
	case p.Threads < 1:
		return refuse("the password hash's settings ask for no threads, but Pisati takes 1 to 255")
	case p.Memory < minMemoryPerThread*uint32(p.Threads) || p.Memory > maxMemory:
		return refuse("the password hash's settings give %s of memory (a KiB is 1,024 bytes) for %s, but Pisati "+
			"takes at least %s for each thread, and at most %s in all", count(uint64(p.Memory), "KiB", "KiB"),
			count(uint64(p.Threads), "thread", "threads"), count(minMemoryPerThread, "KiB", "KiB"),
			count(maxMemory, "KiB", "KiB"))
	}
	return nil
}

// Salt is a hash's salt: 16 bytes, which the caller draws from crypto/rand
// for each hash it makes. The zero Salt is refused, since it is what a
// salt the caller forgot to fill holds.
type Salt [16]byte

// Hash is an argon2id hash of a password, with the salt and params it was
// made with, as HashPassword makes it and ParseHash reads it. PHC gives the
// form to store. Everything else shows only the params: String, fmt (every
// verb), slog and encoding/json show $argon2id$v=19$m=…,t=…,p=…$[redacted],
// also when a Hash sits in another value's unexported field. The zero Hash
// matches no password, and its PHC and String are empty. Hashes do not
// compare with ==, which would compare salts and keys in variable time.
type Hash struct {
	params Params
	// A function rather than the salt and key, for the reason Password
	// holds one: fmt prints a function as its address.
	secret func() (Salt, [keyLen]byte)
}

// newHash returns the Hash of params, s and key, which must be valid.
func newHash(params Params, s Salt, key [keyLen]byte) Hash {
	return Hash{params: params, secret: func() (Salt, [keyLen]byte) { return s, key }}
}

// HashPassword returns p's argon2id hash, made with s and params, with a
// 32-byte key. It returns ErrPasswordTooShort for the zero Password, and an
// error wrapping ErrInvalid for the zero Salt or for params outside the
// bounds Params documents.
//
// It allocates params.Memory KiB, 64 MiB with DefaultParams, and runs on
// params.Threads goroutines of its own until it returns, so callers bound
// how many hashes run at once.
func HashPassword(p Password, s Salt, params Params) (Hash, error) {
	if p.reveal == nil {
		return Hash{}, ErrPasswordTooShort
	}
	if s == (Salt{}) {
		return Hash{}, refuse("auth.HashPassword was given a salt whose 16 bytes are all zero, but a salt is 16 bytes " +
			"from crypto/rand, which is a bug in Pisati")
	}
	if err := params.check(); err != nil {
		return Hash{}, err
	}
	return newHash(params, s, derive(p, s, params)), nil
}

// derive returns argon2id's key for p, s and params, which must be valid.
func derive(p Password, s Salt, params Params) [keyLen]byte {
	return [keyLen]byte(argon2.IDKey([]byte(p.value()), s[:], params.Time, params.Memory, params.Threads, keyLen))
}

// Verify returns Match when p is the password h was made from, and
// Mismatch otherwise. It compares the keys in constant time, so the time
// it takes does not depend on how much of them matched, and it costs as
// much as HashPassword with h's params.
//
// The zero Password and the zero Hash return Mismatch at once, without
// hashing, so the time Verify takes shows whether it had a real hash. A
// caller checking a sign-in for a user that does not exist must therefore
// verify the password against a real hash made with DefaultParams, so
// that the answer takes as long as for a user that does.
func Verify(p Password, h Hash) Verdict {
	if p.reveal == nil || h.secret == nil {
		return Mismatch{}
	}
	salt, want := h.secret()
	got := derive(p, salt, h.params)
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return Mismatch{}
	}
	return Match{NeedsRehash: h.params != DefaultParams()}
}

// Verdict is what Verify decides: Match or Mismatch.
//
//sumtype:decl
type Verdict interface {
	verdict()
}

// Match is a password that matches its hash. NeedsRehash is true when the
// hash was made with other params than DefaultParams: the caller should
// then hash the password again with DefaultParams and store the new hash
// in its place.
type Match struct {
	NeedsRehash bool
}

// Mismatch is a password that does not match its hash.
type Mismatch struct{}

func (Match) verdict()    {}
func (Mismatch) verdict() {}

// The hash's PHC string form: its prefix, then the params, salt and key.
const (
	phcPrefix = "$argon2id$v=19$"
	saltB64   = 22 // 16 bytes, unpadded
	keyB64    = 43 // 32 bytes, unpadded
	// maxPHC is the longest hash ParseHash accepts: the most of each param.
	maxPHC = len(phcPrefix) + len("m=1048576,t=16,p=255") + len("$") + saltB64 + len("$") + keyB64
)

// b64 returns unpadded standard base64, as the PHC string format writes
// it. Strict refuses an encoding whose unused trailing bits are set, so
// that a hash has one spelling.
func b64() *base64.Encoding {
	return base64.RawStdEncoding.Strict()
}

// PHC returns h in the PHC string format, to store, as in
// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>, with the salt and key in
// unpadded standard base64; "" for the zero Hash. ParseHash reads it back
// to a Hash with the same PHC. It is the one way to see h's salt and key.
func (h Hash) PHC() string {
	if h.secret == nil {
		return ""
	}
	salt, key := h.secret()
	return h.params.phc() + b64().EncodeToString(salt[:]) + "$" + b64().EncodeToString(key[:])
}

// String returns h as PHC does but with the salt and key left out, as in
// $argon2id$v=19$m=65536,t=3,p=4$[redacted]; "" for the zero Hash.
func (h Hash) String() string {
	if h.secret == nil {
		return ""
	}
	return h.params.phc() + redacted
}

// phc returns a hash's PHC string as far as the salt: the prefix, p and $.
func (p Params) phc() string {
	return fmt.Sprintf("%sm=%d,t=%d,p=%d$", phcPrefix, p.Memory, p.Time, p.Threads)
}

// Format prints h as String does, whatever the verb and flags.
func (h Hash) Format(f fmt.State, _ rune) {
	io.WriteString(f, h.String()) //nolint:errcheck // fmt.State writes into fmt's own buffer, which cannot fail, and Format has no error to return
}

// MarshalText returns String's form, so encoding/json and slog's handlers,
// which use it, never show the salt or key.
func (h Hash) MarshalText() ([]byte, error) {
	return []byte(h.String()), nil
}

// ParseHash reads s, a hash in the PHC string format that PHC writes, or
// returns an error wrapping ErrInvalid. It is strict: s is argon2id at
// version 19, with exactly the params m, t and p in that order, in decimal
// without leading zeros and within the bounds Params documents, a 16-byte
// salt that is not all zero, and a 32-byte key, each in unpadded standard
// base64 with no unused bits set. So every Hash it returns has the one
// spelling s, which PHC gives back. It refuses an s longer than the
// longest such hash before reading it. Its errors never repeat s.
func ParseHash(s string) (Hash, error) {
	if len(s) > maxPHC {
		return Hash{}, refuse("the stored password hash is %s long, more than the %s that a hash of Pisati's takes",
			count(uint64(len(s)), "byte", "bytes"), count(uint64(maxPHC), "byte", "bytes"))
	}
	rest, ok := strings.CutPrefix(s, phcPrefix)
	if !ok {
		return Hash{}, refuse("the stored password hash does not start with %s, as a hash of Pisati's does", phcPrefix)
	}
	fields := strings.Split(rest, "$")
	if len(fields) != 3 {
		return Hash{}, refuse("the stored password hash does not hold its settings, its salt and its key after %s, "+
			"each after a \"$\"", phcPrefix)
	}
	params, err := parseParams(fields[0])
	if err != nil {
		return Hash{}, err
	}
	var salt Salt
	if len(fields[1]) != saltB64 || !decode(salt[:], fields[1]) {
		return Hash{}, refuse("the stored password hash does not hold a 16-byte salt in %d characters of base64 "+
			"without padding", saltB64)
	}
	if salt == (Salt{}) {
		return Hash{}, refuse("the stored password hash has a salt whose bytes are all zero")
	}
	var key [keyLen]byte
	if len(fields[2]) != keyB64 || !decode(key[:], fields[2]) {
		return Hash{}, refuse("the stored password hash does not hold a 32-byte key in %d characters of base64 "+
			"without padding", keyB64)
	}
	return newHash(params, salt, key), nil
}

// decode reports whether s is exactly len(dst) bytes in b64, and decodes
// it into dst. base64's own error says nothing the caller's does not.
func decode(dst []byte, s string) bool {
	n, err := b64().Decode(dst, []byte(s))
	return err == nil && n == len(dst)
}

// parseParams reads "m=<memory>,t=<time>,p=<threads>".
func parseParams(s string) (Params, error) {
	m, rest, okM := strings.Cut(s, ",")
	t, p, okT := strings.Cut(rest, ",")
	memory, okM2 := number(m, "m=")
	passes, okT2 := number(t, "t=")
	threads, okP := number(p, "p=")
	if !okM || !okT || !okM2 || !okT2 || !okP {
		return Params{}, refuse("the stored password hash does not hold its settings as m=<KiB>,t=<passes>,p=<threads>, " +
			"each a decimal number")
	}
	if threads > 255 {
		return Params{}, refuse("the stored password hash asks for %d threads, but Pisati takes 1 to 255", threads)
	}
	params := Params{Memory: memory, Time: passes, Threads: uint8(threads)}
	if err := params.check(); err != nil {
		return Params{}, err
	}
	return params, nil
}

// number reads s as key followed by a uint32 in decimal, with no sign and
// no leading zero.
func number(s, key string) (uint32, bool) {
	digits, ok := strings.CutPrefix(s, key)
	if !ok || digits == "" || (digits[0] == '0' && len(digits) > 1) {
		return 0, false
	}
	for _, c := range []byte(digits) {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

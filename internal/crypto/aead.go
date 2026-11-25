// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package crypto

import (
	"encoding/binary"
	"math"

	"github.com/divbast/pisati/internal/explain"
)

// The sealed format: a header of the version and the data key's ID, the
// nonce, then the XChaCha20-Poly1305 ciphertext with its tag.
const (
	// version is the format's version, the first byte.
	version = 0x01
	// headerSize is the version and the four-byte key ID.
	headerSize = 1 + 4
	// sealedOverhead is how many bytes longer a sealed value is than
	// what it seals.
	sealedOverhead = headerSize + nonceSize + tagSize

	// maxPlain is the most Seal takes: as much as XChaCha20-Poly1305
	// does, 2^38 - 64 bytes, or less where an int cannot count a sealed
	// value that long.
	maxPlain = min(1<<38-64, math.MaxInt-sealedOverhead)
	// maxAssoc is the most associated data Seal and Open take, so that
	// the header and it fit an int.
	maxAssoc = math.MaxInt - headerSize
)

// Seal returns plain sealed under the current data key: the version byte,
// 0x01; the key's ID, four bytes big-endian; a nonce of 24 bytes from the
// random source; and the XChaCha20-Poly1305 ciphertext of plain with its
// 16-byte tag. The header, version and key ID, is authenticated with
// assoc, as the associated data: Open opens the value only with the same
// assoc, and only as the key it names. So assoc binds the value to where
// it is kept: a caller sealing a registered Field must pass that Field's
// Assoc for the row, which the Reencrypter re-seals it with.
//
// Seal never returns the same value twice, as each has a nonce of its
// own. It does not keep plain or assoc, and returns a value of its own.
// It returns an error when the random source fails, for a Keyring that
// OpenKeyring did not return (wrapping ErrInvalid), and for a plain
// longer than 256 GiB, which XChaCha20-Poly1305 cannot seal.
func (k *Keyring) Seal(plain, assoc []byte) ([]byte, error) {
	if err := k.opened(); err != nil {
		return nil, err
	}
	if len(plain) > maxPlain || len(assoc) > maxAssoc {
		return nil, explain.New(ErrInvalid, "Pisati cannot encrypt a value of %d bytes, with %d bytes of associated data: "+
			"that is more than XChaCha20-Poly1305 takes", len(plain), len(assoc))
	}
	id, dk := k.currentKey()
	var nonce [nonceSize]byte
	if err := k.read(nonce[:]); err != nil {
		return nil, explain.Wrap(err, "Pisati cannot get random bytes to encrypt a value with")
	}
	out := make([]byte, headerSize, sealedOverhead+len(plain))
	out[0] = version
	binary.BigEndian.PutUint32(out[1:headerSize], uint32(id))
	out = append(out, nonce[:]...)
	return dk.aead().Seal(out, nonce[:], plain, associated(out[:headerSize], assoc)), nil
}

// Open returns what sealed holds, once it has authenticated it, header
// and all, with assoc as the associated data. It returns an error wrapping
// ErrTooShort for a value shorter than any Seal makes, ErrUnknownVersion
// for a version other than 0x01, ErrUnknownKey for a key ID the keyring
// does not hold, and ErrNotAuthentic for a value that fails
// authentication: one Seal did not make, one changed since, or one sealed
// with other associated data. Whatever sealed holds, Open does not panic,
// and returns either the plaintext Seal was given or an error. It does
// not keep sealed or assoc, and returns a value of its own, never nil, an
// empty one included. A Keyring that OpenKeyring did not return gets an
// error wrapping ErrInvalid.
func (k *Keyring) Open(sealed, assoc []byte) ([]byte, error) {
	if err := k.opened(); err != nil {
		return nil, err
	}
	const cannot = "Pisati cannot decrypt a value"
	if len(sealed) < sealedOverhead {
		return nil, explain.New(ErrTooShort, "%s: it is %d bytes long, but an encrypted value is at least %d", cannot,
			len(sealed), sealedOverhead)
	}
	if sealed[0] != version {
		return nil, explain.New(ErrUnknownVersion, "%s: it is in format %d, but Pisati knows format %d only", cannot,
			sealed[0], version)
	}
	id := keyID(binary.BigEndian.Uint32(sealed[1:headerSize]))
	dk, ok := k.key(id)
	if !ok {
		return nil, explain.New(ErrUnknownKey, "%s: it is encrypted with the data key %d, which Pisati has not loaded", cannot, id)
	}
	// Seal makes no value longer; XChaCha20-Poly1305 would panic on one.
	const notAuthentic = "it does not pass the check that proves Pisati encrypted it for this place, unchanged"
	if len(sealed)-sealedOverhead > maxPlain || len(assoc) > maxAssoc {
		return nil, explain.New(ErrNotAuthentic, "%s: it is longer than any value Pisati encrypts", cannot)
	}
	nonce, body := sealed[headerSize:headerSize+nonceSize], sealed[headerSize+nonceSize:]
	plain, err := dk.aead().Open(nil, nonce, body, associated(sealed[:headerSize], assoc))
	if err != nil {
		return nil, explain.New(ErrNotAuthentic, "%s: %s", cannot, notAuthentic)
	}
	if plain == nil {
		// The cipher opens an empty value as nil, which a caller would
		// bind as NULL rather than as the empty value it is.
		plain = []byte{}
	}
	return plain, nil
}

// associated returns the associated data a value is sealed with: its
// header, then the caller's assoc. The header comes first and is of a
// fixed size, so no other header and assoc give the same bytes.
func associated(header, assoc []byte) []byte {
	ad := make([]byte, 0, len(header)+len(assoc))
	return append(append(ad, header...), assoc...)
}

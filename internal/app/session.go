// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/core/auth"
	"github.com/divbast/pisati/internal/store"
	"github.com/divbast/pisati/internal/store/storedb"
)

const (
	// httpsCookieName is the session cookie's name over https. A browser
	// takes a cookie with the __Host- prefix only when it is Secure, has
	// Path=/ and no Domain: so it is sent only over https, only to this
	// host, and no other host can set one in its place.
	httpsCookieName = "__Host-pisati"
	// localCookieName is the session cookie's name over plain http on
	// localhost, which serves development alone. A __Host- cookie must be
	// Secure, and Safari refuses a Secure cookie from http://localhost.
	localCookieName = "pisati"

	// idBytes is how long a session ID is: 256 bits from the random
	// source.
	idBytes = 32
	// idChars is how long a session ID is in its cookie: 32 bytes in
	// unpadded base64.
	idChars = 43

	// liveMaxAge is a new session cookie's Max-Age, in seconds: the
	// session's absolute lifetime, which it is made with whole.
	liveMaxAge = int(auth.AbsoluteLimit / time.Second)

	// sweepBatch bounds how many sessions one sweep transaction deletes,
	// so that a sweep holds the write lock only briefly at a time,
	// however many sessions it has to delete.
	sweepBatch = 500
)

// idEncoding returns how a session ID is spelt in its cookie: unpadded
// base64url. Strict refuses a spelling whose two unused bits are set, so
// that an ID has one spelling.
func idEncoding() *base64.Encoding {
	return base64.RawURLEncoding.Strict()
}

// sessionID is a session's ID, which only its cookie holds. It never goes
// to the database or a log: the database keys a session by its key.
type sessionID [idBytes]byte

// parseID returns the ID a cookie value spells, or false unless the value
// is exactly 43 characters of unpadded base64url.
func parseID(value string) (sessionID, bool) {
	var id sessionID
	if len(value) != idChars {
		return id, false
	}
	n, err := idEncoding().Decode(id[:], []byte(value))
	return id, err == nil && n == idBytes
}

// key returns the SHA-256 of id, which the database keys its session by.
// Someone who reads the database, or a backup, learns no ID from it, so
// cannot use any session.
func (id sessionID) key() []byte {
	sum := sha256.Sum256(id[:])
	return sum[:]
}

// expiresAt returns when a session created at created and last seen at
// lastSeen expires, both in Unix milliseconds: whichever comes first of
// auth.AbsoluteLimit after created and auth.IdleLimit after lastSeen.
// Load decides by auth.Evaluate, and Sweep by this.
func expiresAt(created, lastSeen int64) int64 {
	return min(created+auth.AbsoluteLimit.Milliseconds(), lastSeen+auth.IdleLimit.Milliseconds())
}

// Sessions keeps the sessions of signed-in browsers in the database, each
// named by a cookie that holds its ID. The ID is 256 bits from the random
// source, and only its SHA-256 is stored. A session lives for
// auth.IdleLimit after it was last used and for auth.AbsoluteLimit at
// most, as auth.Evaluate decides, and its time of last use is written at
// most once every auth.TouchInterval.
//
// Each cookie it returns has HttpOnly, SameSite=Lax, Path=/ and no
// Domain. Served over https, the cookie is __Host-pisati and Secure; over
// plain http on localhost, for development, it is pisati, without Secure.
//
// Its methods are safe for concurrent use. The zero Sessions has no
// database: its methods return an error.
type Sessions struct {
	store  *store.Store
	clock  func() time.Time
	rnd    io.Reader
	name   string
	secure bool
}

// NewSessions returns Sessions kept in s, reading the time from clock and
// drawing IDs from rnd, which must be safe for concurrent use, as
// crypto/rand's Reader is. Its cookies are named, and Secure or not, as
// Sessions describes for base's scheme. It returns an error for a store
// Open did not return, a nil clock or rnd, or the zero base.
func NewSessions(s *store.Store, clock func() time.Time, rnd io.Reader, base config.BaseURL) (*Sessions, error) {
	var errs []error
	if s.DB() == nil {
		errs = append(errs, errors.New("app: sessions need a store made by store.Open"))
	}
	if clock == nil {
		errs = append(errs, errors.New("app: sessions need a clock"))
	}
	if rnd == nil {
		errs = append(errs, errors.New("app: sessions need a random source"))
	}
	if base == (config.BaseURL{}) {
		errs = append(errs, errors.New("app: sessions need the base URL, and the zero BaseURL is none"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	name, secure := localCookieName, false
	if base.IsHTTPS() {
		name, secure = httpsCookieName, true
	}
	return &Sessions{store: s, clock: clock, rnd: rnd, name: name, secure: secure}, nil
}

// CookieName returns the name of the session cookie: __Host-pisati over
// https, and pisati over plain http on localhost. It is "" for the zero
// Sessions.
func (s *Sessions) CookieName() string {
	if s == nil {
		return ""
	}
	return s.name
}

// Clear returns a cookie that expires the session cookie at once: of the
// same name and attributes, empty and with Max-Age=0. It touches no
// database, so it serves for a cookie that names no session.
func (s *Sessions) Clear() http.Cookie {
	return s.cookie("", -1)
}

// cookie returns the session cookie with value and maxAge in seconds,
// with the attributes Sessions describes.
func (s *Sessions) cookie(value string, maxAge int) http.Cookie {
	//nolint:gosec // G124: Secure is false only for a plain-http base URL, which config.ParseBaseURL allows on localhost alone, for development, where Safari refuses a Secure cookie.
	return http.Cookie{
		Name:     s.CookieName(),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   s != nil && s.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// made returns an error for Sessions that NewSessions did not make.
func (s *Sessions) made() error {
	if s == nil || s.store == nil {
		return errors.New("app: the Sessions was not made by NewSessions")
	}
	return nil
}

// Session is a live session, as Load finds it. The zero Session is no
// session: it has no user.
type Session struct {
	user auth.UserID
}

// User returns the user the session is signed in as, or false when it
// has none.
func (s Session) User() (auth.UserID, bool) {
	return s.user, s.user > 0
}

// sessionOf returns the Session of a row of the sessions table.
func sessionOf(row storedb.Session) Session {
	if !row.UserID.Valid || row.UserID.Int64 <= 0 {
		return Session{}
	}
	return Session{user: auth.UserID(row.UserID.Int64)}
}

// Load returns the live session that the cookie value names, or false
// when it names none: a value that is not 43 characters of unpadded
// base64url, which it refuses without reading the database, an ID with
// no session, or a session that has expired, which it deletes. A live
// session last seen auth.TouchInterval or more ago is touched: its time
// of last use set to now, and its expiry moved to match. Concurrent loads
// of one session touch it once. A failure to read, touch or delete the
// session is an error, never taken for no session, and its errors never
// hold the value.
func (s *Sessions) Load(ctx context.Context, value string) (Session, bool, error) {
	if err := s.made(); err != nil {
		return Session{}, false, err
	}
	id, ok := parseID(value)
	if !ok {
		return Session{}, false, nil
	}
	key := id.key()
	q := s.store.Queries()
	row, err := q.GetSession(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("app: load a session: %w", err)
	}

	now := s.clock()
	times := auth.SessionTimes{Created: time.UnixMilli(row.CreatedAt), LastSeen: time.UnixMilli(row.LastSeenAt)}
	switch state := auth.Evaluate(times, now).(type) {
	case auth.Live:
		if state.TouchDue {
			// The condition on last_seen_at has a touch that another
			// load made first, since this one read the row, win: the
			// session is written once however many requests find a
			// touch due.
			if _, err := q.TouchSession(ctx, storedb.TouchSessionParams{
				LastSeenAt:  now.UnixMilli(),
				ExpiresAt:   expiresAt(row.CreatedAt, now.UnixMilli()),
				IDHash:      key,
				TouchBefore: now.Add(-auth.TouchInterval).UnixMilli(),
			}); err != nil {
				return Session{}, false, fmt.Errorf("app: touch a session: %w", err)
			}
		}
		return sessionOf(row), true, nil
	case auth.IdleExpired, auth.AbsoluteExpired:
		if err := q.DeleteSession(ctx, key); err != nil {
			return Session{}, false, fmt.Errorf("app: delete an expired session: %w", err)
		}
		return Session{}, false, nil
	default:
		// Only a type outside auth that embeds a state reaches here: a
		// bug, which must sign no one in.
		return Session{}, false, fmt.Errorf("app: load a session: auth.Evaluate returned an unknown %T", state)
	}
}

// Renew starts a new session for user, which must be positive, and
// returns its cookie, with Max-Age the session's absolute lifetime. Every
// call makes a session with a new ID and leaves every other session as
// it is: a caller signing a user in, or changing what a user may do,
// destroys the session the request came with first, so that an ID known
// before never names the new session. It returns an error when the
// random source fails, or the session cannot be stored.
func (s *Sessions) Renew(ctx context.Context, user auth.UserID) (http.Cookie, error) {
	if err := s.made(); err != nil {
		return http.Cookie{}, err
	}
	if user <= 0 {
		return http.Cookie{}, fmt.Errorf("app: start a session: user ID %d is not positive", user)
	}
	var id sessionID
	if _, err := io.ReadFull(s.rnd, id[:]); err != nil {
		return http.Cookie{}, fmt.Errorf("app: draw a session ID: %w", err)
	}
	// All zero is what a source that never filled the buffer leaves.
	if id == (sessionID{}) {
		return http.Cookie{}, errors.New("app: draw a session ID: the random source gave all zero bytes")
	}
	now := s.clock().UnixMilli()
	if err := s.store.Queries().InsertSession(ctx, storedb.InsertSessionParams{
		IDHash:     id.key(),
		UserID:     sql.NullInt64{Int64: int64(user), Valid: true},
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  expiresAt(now, now),
	}); err != nil {
		return http.Cookie{}, fmt.Errorf("app: store a session: %w", err)
	}
	return s.cookie(idEncoding().EncodeToString(id[:]), liveMaxAge), nil
}

// Destroy deletes the session the cookie value names, if there is one,
// and returns Clear's cookie, which expires it in the browser too. A
// value that names no session is no error.
func (s *Sessions) Destroy(ctx context.Context, value string) (http.Cookie, error) {
	if err := s.made(); err != nil {
		return http.Cookie{}, err
	}
	if id, ok := parseID(value); ok {
		if err := s.store.Queries().DeleteSession(ctx, id.key()); err != nil {
			return http.Cookie{}, fmt.Errorf("app: destroy a session: %w", err)
		}
	}
	return s.Clear(), nil
}

// DestroyAllForUser deletes every session of user, which must be
// positive: it signs the user out everywhere.
func (s *Sessions) DestroyAllForUser(ctx context.Context, user auth.UserID) error {
	if err := s.made(); err != nil {
		return err
	}
	if user <= 0 {
		return fmt.Errorf("app: destroy a user's sessions: user ID %d is not positive", user)
	}
	if _, err := s.store.Queries().DeleteUserSessions(ctx, sql.NullInt64{Int64: int64(user), Valid: true}); err != nil {
		return fmt.Errorf("app: destroy a user's sessions: %w", err)
	}
	return nil
}

// Sweep deletes every session that has expired by now, whether or not its
// cookie is ever presented again, and returns how many it deleted. It
// deletes at most 500 in each write transaction, so that other writers
// get the lock between them. On an error it returns the error and how
// many it had deleted before it.
func (s *Sessions) Sweep(ctx context.Context) (int, error) {
	if err := s.made(); err != nil {
		return 0, err
	}
	now := s.clock().UnixMilli()
	removed := 0
	for {
		var n int64
		err := s.store.InTx(ctx, func(q *storedb.Queries) error {
			var err error
			n, err = q.SweepSessions(ctx, storedb.SweepSessionsParams{Now: now, Batch: sweepBatch})
			return err
		})
		if err != nil {
			return removed, fmt.Errorf("app: sweep sessions: %w", err)
		}
		removed += int(n)
		if n < sweepBatch {
			return removed, nil
		}
	}
}

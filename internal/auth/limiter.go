// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"math"
	"sync"
	"time"
)

// LoginLimiter slows down password guessing against basic auth. Each key
// (a username, or a client address) has an allowance of failed sign-ins
// that refills over time; once it is used up, further attempts are refused
// without checking the password, until it refills. A successful sign-in
// restores the username's allowance.
type LoginLimiter struct {
	failures float64       // allowance: failures before refusing
	refill   time.Duration // one more failure allowed per refill
	now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	sweptAt time.Time
}

type bucket struct {
	tokens float64 // failures still allowed
	at     time.Time
}

// maxBuckets bounds the limiter's memory: keys that have refilled are
// dropped first; past this, the oldest are.
const maxBuckets = 100_000

// NewLoginLimiter allows failures failed sign-ins per key, one more per
// refill. failures <= 0 turns limiting off (nil).
func NewLoginLimiter(failures int, refill time.Duration) *LoginLimiter {
	if failures <= 0 {
		return nil
	}
	if refill <= 0 {
		refill = time.Minute
	}
	return &LoginLimiter{failures: float64(failures), refill: refill, now: time.Now, buckets: map[string]*bucket{}}
}

// ErrTooManyAttempts is returned while a key's allowance is used up.
type ErrTooManyAttempts struct{ RetryAfter time.Duration }

func (e *ErrTooManyAttempts) Error() string { return errTooMany.Error() }

// Is makes errors.Is(err, errTooMany) match.
func (e *ErrTooManyAttempts) Is(target error) bool { return target == errTooMany }

var errTooMany = errors.New("too many failed sign-ins")

// IsTooManyAttempts reports whether err is a refusal by the limiter, and
// when to retry.
func IsTooManyAttempts(err error) (time.Duration, bool) {
	var e *ErrTooManyAttempts
	if errors.As(err, &e) {
		return e.RetryAfter, true
	}
	return 0, false
}

// level refills b up to now (lock held).
func (l *LoginLimiter) level(b *bucket, now time.Time) {
	b.tokens = math.Min(l.failures, b.tokens+float64(now.Sub(b.at))/float64(l.refill))
	b.at = now
}

// Check refuses if any key's allowance is used up, saying when the first
// failure will be allowed again.
func (l *LoginLimiter) Check(keys ...string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wait time.Duration
	for _, k := range keys {
		b := l.buckets[k]
		if b == nil {
			continue
		}
		l.level(b, now)
		if b.tokens < 1 {
			wait = max(wait, time.Duration((1-b.tokens)*float64(l.refill)))
		}
	}
	if wait > 0 {
		return &ErrTooManyAttempts{RetryAfter: wait.Round(time.Second) + time.Second}
	}
	return nil
}

// Fail records a failed sign-in for every key.
func (l *LoginLimiter) Fail(keys ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	for _, k := range keys {
		b := l.buckets[k]
		if b == nil {
			b = &bucket{tokens: l.failures, at: now}
			l.buckets[k] = b
		}
		l.level(b, now)
		b.tokens = math.Max(0, b.tokens-1)
	}
}

// Reset restores a key's allowance (after a successful sign-in).
func (l *LoginLimiter) Reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}

// sweep drops keys whose allowance has refilled, at most once a minute, and
// the oldest keys if there are still too many (lock held).
func (l *LoginLimiter) sweep(now time.Time) {
	if len(l.buckets) < maxBuckets && now.Sub(l.sweptAt) < time.Minute {
		return
	}
	l.sweptAt = now
	full := time.Duration(l.failures * float64(l.refill))
	for k, b := range l.buckets {
		if now.Sub(b.at) >= full {
			delete(l.buckets, k)
		}
	}
	for len(l.buckets) >= maxBuckets {
		var oldest string
		var at time.Time
		for k, b := range l.buckets {
			if oldest == "" || b.at.Before(at) {
				oldest, at = k, b.at
			}
		}
		delete(l.buckets, oldest)
	}
}

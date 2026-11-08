// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// usersFile is the users.yaml format.
//
//	users:
//	  - username: alice
//	    password_hash: $2a$10$...   # from `nbsandboxd hash-password`
type usersFile struct {
	Users []struct {
		Username     string `yaml:"username"`
		PasswordHash string `yaml:"password_hash"`
		// Admin gives the user the nbsandbox.admin scope (managing every
		// owner's spaces). Off unless set.
		Admin bool `yaml:"admin"`
	} `yaml:"users"`
}

var usernameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Users verifies credentials against bcrypt hashes loaded from a file.
type Users struct {
	hashes map[string][]byte
	admins map[string]bool
	cache  *verifiedCache
}

// Credential cache. bcrypt costs tens of milliseconds of CPU,
// paid on every request with basic auth. A successful check is remembered for
// a short time, keyed by an HMAC of username and password under a random
// per-process key: the cache never holds a password or anything that could
// be checked offline after the process ends. Failed checks are never cached,
// so a wrong password always pays the full bcrypt cost.
const (
	cacheTTL        = 60 * time.Second
	cacheMaxEntries = 10_000
)

type verifiedCache struct {
	key     []byte
	now     func() time.Time
	mu      sync.Mutex
	entries map[[32]byte]time.Time // -> expiry
}

func newVerifiedCache() *verifiedCache {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return &verifiedCache{key: key, now: time.Now, entries: map[[32]byte]time.Time{}}
}

func (c *verifiedCache) mac(username, password string) [32]byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(username))
	m.Write([]byte{0})
	m.Write([]byte(password))
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

func (c *verifiedCache) hit(k [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[k]
	if ok && c.now().Before(exp) {
		return true
	}
	delete(c.entries, k)
	return false
}

func (c *verifiedCache) add(k [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= cacheMaxEntries {
		clear(c.entries)
	}
	c.entries[k] = c.now().Add(cacheTTL)
}

// dummyHash is compared against when the username is unknown, so a request
// for a missing user takes as long as one with a wrong password and the
// response time doesn't reveal which usernames exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("nbsandbox-dummy-password"), bcrypt.DefaultCost)

// LoadUsers reads and validates the users file. It refuses a file that other
// users on the host can read or write.
func LoadUsers(path string) (*Users, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		return nil, fmt.Errorf("users file %s is accessible to other users (mode %04o); run: chmod o-rwx %s", path, perm, path)
	}
	b, err := os.ReadFile(path) //nolint:gosec // path comes from the operator
	if err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	return ParseUsers(b)
}

// ParseUsers parses users.yaml content.
func ParseUsers(b []byte) (*Users, error) {
	var f usersFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	u := &Users{hashes: make(map[string][]byte, len(f.Users)), admins: map[string]bool{}, cache: newVerifiedCache()}
	var errs []error
	for i, e := range f.Users {
		switch {
		case !usernameRE.MatchString(e.Username):
			errs = append(errs, fmt.Errorf("users[%d]: invalid username %q (lowercase letters, digits, - and _)", i, e.Username))
		case u.hashes[e.Username] != nil:
			errs = append(errs, fmt.Errorf("users[%d]: duplicate username %q", i, e.Username))
		default:
			if _, err := bcrypt.Cost([]byte(e.PasswordHash)); err != nil {
				errs = append(errs, fmt.Errorf("users[%d] (%s): password_hash is not a bcrypt hash", i, e.Username))
				continue
			}
			u.hashes[e.Username] = []byte(e.PasswordHash)
			u.admins[e.Username] = e.Admin
		}
	}
	if len(f.Users) == 0 {
		errs = append(errs, errors.New("users file: no users defined"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return u, nil
}

// demoHashes are the password hashes of the demo users in config/users.yaml
// (passwords published in that file).
var demoHashes = map[string]bool{
	"$2a$10$TJpv7hAD5qHTWT3Nmmm/K.t9ZUoUckHAXnbc1FK2HIkwPfBEdw3l2": true, // alice
	"$2a$10$ssJfzp0aX.qw5vc3Ie0aAuNwj2WDXj7lOcVUblcb7Rfex.IQ9xY6S": true, // bob
}

// DemoUsers lists the users whose password is a published demo password
// (the repository's config/users.yaml), sorted. nbsandboxd warns about them:
// fine for local development, never for a deployment.
func (u *Users) DemoUsers() []string {
	var out []string
	for name, h := range u.hashes {
		if demoHashes[string(h)] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Verify reports whether the username and password are valid. Unless the
// same credentials passed recently, it does one bcrypt comparison, whether or
// not the user exists.
func (u *Users) Verify(username, password string) bool {
	k := u.cache.mac(username, password)
	if u.cache.hit(k) {
		return true
	}
	hash, known := u.hashes[username]
	if !known {
		hash = dummyHash
	}
	ok := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil && known
	if ok {
		u.cache.add(k)
	}
	return ok
}

// Count returns the number of configured users.
func (u *Users) Count() int { return len(u.hashes) }

// Admin reports whether username is an administrator.
func (u *Users) Admin(username string) bool { return u != nil && u.admins[username] }

// Has reports whether username is configured (for the web UI).
func (u *Users) Has(username string) bool { _, ok := u.hashes[username]; return ok }

// Names returns the configured usernames, sorted.
func (u *Users) Names() []string {
	out := make([]string, 0, len(u.hashes))
	for n := range u.hashes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HashPassword returns a bcrypt hash for users.yaml.
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}

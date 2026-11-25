// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// BuiltinStore is where the built-in provider keeps encrypted values
// (nbsandbox's database).
type BuiltinStore interface {
	PutBuiltinSecret(ctx context.Context, id string, nonce, ciphertext []byte, at time.Time) error
	GetBuiltinSecret(ctx context.Context, id string) (nonce, ciphertext []byte, err error)
	DeleteBuiltinSecret(ctx context.Context, id string) error
}

// Builtin keeps credentials encrypted (AES-256-GCM) in nbsandbox's database,
// with a master key the operator provides. For development: production keeps
// them in Kubernetes Secrets or Vault.
type Builtin struct {
	aead  cipher.AEAD
	store BuiltinStore
	now   func() time.Time
}

// NewBuiltin returns the built-in provider; masterKey is 32 bytes, base64.
func NewBuiltin(masterKey string, store BuiltinStore) (*Builtin, error) {
	key, err := base64.StdEncoding.DecodeString(masterKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("the built-in secret store needs a 32-byte master key, base64")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Builtin{aead: aead, store: store, now: time.Now}, nil
}

// Name implements Provider.
func (b *Builtin) Name() string { return "builtin" }

func builtinRef(connectionID string) string { return "conn-" + connectionID }

// Store implements Provider. The reference is bound into the encryption, so
// a stored value can't be moved to another connection.
func (b *Builtin) Store(ctx context.Context, connectionID string, value []byte) (string, error) {
	ref := builtinRef(connectionID)
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nil, nonce, value, []byte(ref))
	if err := b.store.PutBuiltinSecret(ctx, ref, nonce, ct, b.now()); err != nil {
		return "", err
	}
	return ref, nil
}

// Remove implements Provider.
func (b *Builtin) Remove(ctx context.Context, ref string) error {
	return b.store.DeleteBuiltinSecret(ctx, ref)
}

// Check implements Provider: the built-in store has no references.
func (b *Builtin) Check(context.Context, string) error { return ErrNoReferences }

// Reveal decrypts a stored value, for the development fetcher, which
// receives it the way a Kubernetes Pod would mount it.
func (b *Builtin) Reveal(ctx context.Context, ref string) ([]byte, error) {
	nonce, ct, err := b.store.GetBuiltinSecret(ctx, ref)
	if err != nil {
		return nil, err
	}
	v, err := b.aead.Open(nil, nonce, ct, []byte(ref))
	if err != nil {
		return nil, fmt.Errorf("secret %s can't be decrypted (another master key?): %w", ref, err)
	}
	return v, nil
}

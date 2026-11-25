// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type memStore struct {
	mu sync.Mutex
	m  map[string][2][]byte
}

func (s *memStore) PutBuiltinSecret(_ context.Context, id string, nonce, ct []byte, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = [2][]byte{nonce, ct}
	return nil
}

func (s *memStore) GetBuiltinSecret(_ context.Context, id string) ([]byte, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok {
		return nil, nil, errors.New("not found")
	}
	return v[0], v[1], nil
}

func (s *memStore) DeleteBuiltinSecret(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

func key(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(b), 32)))
}

func TestBuiltin(t *testing.T) {
	ctx := context.Background()
	st := &memStore{m: map[string][2][]byte{}}
	if _, err := NewBuiltin("too-short", st); err == nil {
		t.Fatal("a bad master key was accepted")
	}
	b, err := NewBuiltin(key('a'), st)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := b.Store(ctx, "c1", []byte("pa55word"))
	if err != nil {
		t.Fatal(err)
	}
	if v := st.m[ref][1]; strings.Contains(string(v), "pa55word") {
		t.Fatal("stored in the clear")
	}
	if v, err := b.Reveal(ctx, ref); err != nil || string(v) != "pa55word" {
		t.Fatalf("reveal: %q %v", v, err)
	}
	// A value moved to another connection's reference doesn't decrypt.
	st.m["conn-c2"] = st.m[ref]
	if _, err := b.Reveal(ctx, "conn-c2"); err == nil {
		t.Fatal("a value moved between connections decrypted")
	}
	// Another master key can't read it.
	other, _ := NewBuiltin(key('b'), st)
	if _, err := other.Reveal(ctx, ref); err == nil {
		t.Fatal("another master key decrypted it")
	}
	if err := b.Check(ctx, "x"); !errors.Is(err, ErrNoReferences) {
		t.Fatalf("references: %v", err)
	}
	if err := b.Remove(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reveal(ctx, ref); err == nil {
		t.Fatal("still there after remove")
	}
}

func TestKubernetes(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "crm-prod", Namespace: "runs"}, Data: map[string][]byte{"password": []byte("x")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "nbsandbox-conn-squat", Namespace: "runs"}, Data: map[string][]byte{"secret": []byte("theirs")}},
	)
	k := NewKubernetes(client, "runs")
	ref, err := k.Store(ctx, "c1", []byte("v1"))
	if err != nil || ref != "nbsandbox-conn-c1#secret" {
		t.Fatalf("store: %q %v", ref, err)
	}
	if _, err := k.Store(ctx, "c1", []byte("v2")); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	sec, _ := client.CoreV1().Secrets("runs").Get(ctx, "nbsandbox-conn-c1", metav1.GetOptions{})
	if string(sec.Data["secret"]) != "v2" || sec.Labels[LabelManagedBy] != "nbsandbox" || sec.Labels[LabelConnection] != "c1" {
		t.Fatalf("managed secret: %+v", sec)
	}
	// A Secret with the same name that isn't nbsandbox's is never overwritten.
	if _, err := k.Store(ctx, "squat", []byte("mine")); err == nil {
		t.Fatal("overwrote someone else's secret")
	}
	// References: checked, never read out.
	if err := k.Check(ctx, "crm-prod#password"); err != nil {
		t.Fatalf("existing reference: %v", err)
	}
	for _, bad := range []string{"crm-prod#nope", "missing#password", "no-key", "Bad_Name#k", "a#bad/key"} {
		if err := k.Check(ctx, bad); !errors.Is(err, ErrBadReference) {
			t.Errorf("Check(%q): %v", bad, err)
		}
	}
	// Remove deletes only nbsandbox's own Secrets.
	if err := k.Remove(ctx, "crm-prod#password"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("runs").Get(ctx, "crm-prod", metav1.GetOptions{}); err != nil {
		t.Fatal("removed a referenced secret")
	}
	if err := k.Remove(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("runs").Get(ctx, "nbsandbox-conn-c1", metav1.GetOptions{}); err == nil {
		t.Fatal("the managed secret is still there")
	}
	if err := k.Remove(ctx, ref); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
}

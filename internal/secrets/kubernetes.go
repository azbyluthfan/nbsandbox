// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

// Labels on the Secrets nbsandbox writes: it removes only these.
const (
	LabelManagedBy  = "app.kubernetes.io/managed-by"
	labelManagedBy  = "nbsandbox"
	LabelConnection = "nbsandbox.connection"
	managedKey      = "secret"
)

// Kubernetes keeps credentials in Kubernetes Secrets in one namespace: the
// runs namespace, since a Pod can mount only its own namespace's Secrets.
// Managed values go in Secrets nbsandbox creates and labels; a referenced
// one is "name#key", an existing Secret in that namespace (written by GitOps,
// Sealed Secrets or External Secrets Operator).
type Kubernetes struct {
	client    kubernetes.Interface
	namespace string
}

// NewKubernetes returns the Kubernetes provider for namespace.
func NewKubernetes(client kubernetes.Interface, namespace string) *Kubernetes {
	return &Kubernetes{client: client, namespace: namespace}
}

// Name implements Provider.
func (k *Kubernetes) Name() string { return "kubernetes" }

// Namespace is where the provider's Secrets are.
func (k *Kubernetes) Namespace() string { return k.namespace }

func managedName(connectionID string) string { return "nbsandbox-conn-" + connectionID }

// ParseRef splits a reference into the Secret's name and the key within it.
func ParseRef(ref string) (name, key string, err error) {
	name, key, ok := strings.Cut(ref, "#")
	if !ok || len(validation.IsDNS1123Subdomain(name)) > 0 || len(validation.IsConfigMapKey(key)) > 0 {
		return "", "", fmt.Errorf("%w: %q (expected secret-name#key, a Secret in the runs namespace)", ErrBadReference, ref)
	}
	return name, key, nil
}

// Store implements Provider.
func (k *Kubernetes) Store(ctx context.Context, connectionID string, value []byte) (string, error) {
	name := managedName(connectionID)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.namespace,
			Labels: map[string]string{LabelManagedBy: labelManagedBy, LabelConnection: connectionID}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{managedKey: value},
	}
	secrets := k.client.CoreV1().Secrets(k.namespace)
	_, err := secrets.Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		cur, gerr := secrets.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return "", gerr
		}
		if cur.Labels[LabelManagedBy] != labelManagedBy {
			return "", fmt.Errorf("secret %s/%s exists and isn't nbsandbox's: not overwriting it", k.namespace, name)
		}
		cur.Data = sec.Data
		_, err = secrets.Update(ctx, cur, metav1.UpdateOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("store the connection's secret: %w", err)
	}
	return name + "#" + managedKey, nil
}

// Remove implements Provider: only a Secret nbsandbox created and labelled.
func (k *Kubernetes) Remove(ctx context.Context, ref string) error {
	name, _, err := ParseRef(ref)
	if err != nil {
		return err
	}
	secrets := k.client.CoreV1().Secrets(k.namespace)
	cur, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Labels[LabelManagedBy] != labelManagedBy {
		return nil // a referenced Secret: someone else's to delete
	}
	err = secrets.Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Check implements Provider: the Secret exists in the namespace and has the
// key. Its value is read by the API call but never kept or returned.
func (k *Kubernetes) Check(ctx context.Context, ref string) error {
	name, key, err := ParseRef(ref)
	if err != nil {
		return err
	}
	cur, err := k.client.CoreV1().Secrets(k.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: no secret %q in namespace %s", ErrBadReference, name, k.namespace)
	}
	if err != nil {
		return err
	}
	if _, ok := cur.Data[key]; !ok {
		return fmt.Errorf("%w: secret %q has no key %q", ErrBadReference, name, key)
	}
	return nil
}

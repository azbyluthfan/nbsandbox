// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package azstore is the blob.Store for Azure Blob Storage (a storage
// account's container). On AKS it authenticates with Workload Identity (or
// any identity azidentity's default chain finds), so no storage keys are
// stored anywhere; a connection string is for development (Azurite).
package azstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
)

// Config locates the container.
type Config struct {
	// AccountURL is https://<account>.blob.core.windows.net (with an
	// identity), or empty when ConnectionString is set.
	AccountURL string `yaml:"account_url"`
	// ConnectionString authenticates with an account key (development,
	// Azurite). It is a secret: prefer the environment variable.
	ConnectionString string `yaml:"connection_string"`
	// Container holds the objects; Prefix (optional) puts them under a
	// folder of a shared container.
	Container string `yaml:"container"`
	Prefix    string `yaml:"prefix"`
	// CreateContainer creates the container if it doesn't exist.
	CreateContainer bool `yaml:"create_container"`
}

// Store is an Azure Blob Storage container (or a prefix of one).
type Store struct {
	c         *azblob.Client
	container string
	prefix    string
}

// New connects to the container, creating it if asked to.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Container == "" {
		return nil, errors.New("azure: container is required")
	}
	if cfg.Prefix != "" && !blob.ValidPrefix(cfg.Prefix) {
		return nil, fmt.Errorf("azure: prefix %q must be lowercase path segments ending in /", cfg.Prefix)
	}
	var c *azblob.Client
	var err error
	switch {
	case cfg.ConnectionString != "":
		c, err = azblob.NewClientFromConnectionString(cfg.ConnectionString, nil)
	case strings.HasPrefix(cfg.AccountURL, "https://"):
		cred, cerr := azidentity.NewDefaultAzureCredential(nil)
		if cerr != nil {
			return nil, fmt.Errorf("azure: identity: %w", cerr)
		}
		c, err = azblob.NewClient(cfg.AccountURL, cred, nil)
	default:
		return nil, errors.New("azure: set account_url (https://<account>.blob.core.windows.net) or connection_string")
	}
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	if cfg.CreateContainer {
		if _, err := c.CreateContainer(ctx, cfg.Container, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			return nil, fmt.Errorf("azure: create container %s: %w", cfg.Container, err)
		}
	}
	p := cfg.Prefix
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	s := &Store{c: c, container: cfg.Container, prefix: p}
	if err := s.Ping(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Ping checks the container is reachable (readiness).
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.c.ServiceClient().NewContainerClient(s.container).GetProperties(ctx, nil); err != nil {
		return fmt.Errorf("azure: container %s: %w", s.container, err)
	}
	return nil
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if !blob.ValidKey(key) {
		return &blob.KeyError{Key: key}
	}
	if size < 0 {
		return blob.ErrSizeMismatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A block blob is committed only when the whole stream has been read,
	// so a reader that fails part-way stores nothing.
	er := blob.NewExactReader(r, size)
	_, err := s.c.UploadStream(ctx, s.container, s.prefix+key, er, nil)
	if er.Mismatch() {
		return blob.ErrSizeMismatch
	}
	if err != nil {
		return fmt.Errorf("azure: put %s: %w", key, err)
	}
	return nil
}

// Get implements blob.Store.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !blob.ValidKey(key) {
		return nil, &blob.KeyError{Key: key}
	}
	resp, err := s.c.DownloadStream(ctx, s.container, s.prefix+key, nil)
	if err != nil {
		return nil, mapErr(key, err)
	}
	return resp.Body, nil
}

// Head implements blob.Store.
func (s *Store) Head(ctx context.Context, key string) (blob.Info, error) {
	if !blob.ValidKey(key) {
		return blob.Info{}, &blob.KeyError{Key: key}
	}
	p, err := s.c.ServiceClient().NewContainerClient(s.container).NewBlobClient(s.prefix+key).GetProperties(ctx, nil)
	if err != nil {
		return blob.Info{}, mapErr(key, err)
	}
	info := blob.Info{Key: key}
	if p.ContentLength != nil {
		info.Size = *p.ContentLength
	}
	if p.LastModified != nil {
		info.Modified = *p.LastModified
	}
	return info, nil
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	if !blob.ValidKey(key) {
		return &blob.KeyError{Key: key}
	}
	if _, err := s.c.DeleteBlob(ctx, s.container, s.prefix+key, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("azure: delete %s: %w", key, err)
	}
	return nil
}

// List implements blob.Store.
func (s *Store) List(ctx context.Context, prefix string, fn func(blob.Info) error) error {
	if !blob.ValidPrefix(prefix) {
		return &blob.KeyError{Key: prefix}
	}
	full := s.prefix + prefix
	pager := s.c.NewListBlobsFlatPager(s.container, &azblob.ListBlobsFlatOptions{Prefix: &full})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("azure: list %s: %w", prefix, err)
		}
		for _, b := range page.Segment.BlobItems {
			if b.Name == nil {
				continue
			}
			info := blob.Info{Key: strings.TrimPrefix(*b.Name, s.prefix)}
			if b.Properties != nil && b.Properties.ContentLength != nil {
				info.Size = *b.Properties.ContentLength
			}
			if b.Properties != nil && b.Properties.LastModified != nil {
				info.Modified = *b.Properties.LastModified
			}
			if err := fn(info); err != nil {
				return err
			}
		}
	}
	return nil
}

func mapErr(key string, err error) error {
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return blob.ErrNotFound
	}
	return fmt.Errorf("azure: %s: %w", key, err)
}

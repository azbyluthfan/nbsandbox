// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package s3store is the blob.Store for S3-compatible object storage:
// RustFS (the default in this repository), Ceph (RGW), SeaweedFS, AWS S3,
// Cloudflare R2 and others. It uses the AWS SDK for Go v2 with path-style
// requests, which every S3-compatible store accepts.
package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/azbyluthfan/nbsandbox/internal/blob"
)

// Config locates the bucket.
type Config struct {
	// Endpoint is host[:port], without a scheme (e.g. s3.amazonaws.com,
	// rustfs:9000).
	Endpoint string `yaml:"endpoint"`
	// TLS uses https (default for anything but a local endpoint).
	TLS bool `yaml:"tls"`
	// Region, when the store needs one (AWS).
	Region string `yaml:"region"`
	// Bucket holds the objects; Prefix (optional, e.g. "nbsandbox/") puts
	// them under a folder of a shared bucket.
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
	// AccessKey and SecretKey; when empty, the standard environment
	// variables (AWS_ACCESS_KEY_ID, ...) or the cloud's instance identity
	// are used.
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// CreateBucket creates the bucket if it doesn't exist (development).
	CreateBucket bool `yaml:"create_bucket"`
}

// Store is an S3 bucket (or a prefix of one).
type Store struct {
	c      *s3.Client
	bucket string
	prefix string
}

// New connects to the bucket, creating it if asked to.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3: endpoint and bucket are required")
	}
	if strings.Contains(cfg.Endpoint, "://") {
		return nil, errors.New("s3: endpoint is host[:port], without http:// or https:// (set tls instead)")
	}
	if cfg.Prefix != "" && !blob.ValidPrefix(cfg.Prefix) {
		return nil, fmt.Errorf("s3: prefix %q must be lowercase path segments ending in /", cfg.Prefix)
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1" // what S3-compatible stores expect when they don't care
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if cfg.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}
	// Otherwise the standard chain: AWS_* environment variables, shared
	// files, web identity (Kubernetes), instance roles.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	scheme := "http://"
	if cfg.TLS {
		scheme = "https://"
	}
	c := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(scheme + cfg.Endpoint)
		o.UsePathStyle = true
		// Checksums only where S3 requires them: not every compatible
		// store supports the newer trailing checksums.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	s := &Store{c: c, bucket: cfg.Bucket, prefix: cfg.Prefix}
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		if !notFound(err) {
			return nil, fmt.Errorf("s3: bucket %s: %w", cfg.Bucket, err)
		}
		if !cfg.CreateBucket {
			return nil, fmt.Errorf("s3: bucket %s does not exist", cfg.Bucket)
		}
		in := &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)}
		if region != "us-east-1" {
			in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
		}
		if _, err := c.CreateBucket(ctx, in); err != nil {
			return nil, fmt.Errorf("s3: create bucket %s: %w", cfg.Bucket, err)
		}
	}
	return s, nil
}

// Ping checks the bucket is reachable (readiness).
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if !blob.ValidKey(key) {
		return &blob.KeyError{Key: key}
	}
	if size < 0 {
		return blob.ErrSizeMismatch
	}
	er := blob.NewExactReader(r, size)
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key), Body: er,
		ContentLength: aws.Int64(size), ContentType: aws.String("application/octet-stream"),
	}, // The body streams (it isn't seekable): sign the headers, not a hash
		// of the payload; the content address is checked on every read.
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware))
	if er.Mismatch() {
		// The upload failed on our error, so nothing was stored.
		return blob.ErrSizeMismatch
	}
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	return nil
}

// Get implements blob.Store.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !blob.ValidKey(key) {
		return nil, &blob.KeyError{Key: key}
	}
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key)})
	if err != nil {
		return nil, mapErr(key, err)
	}
	return out.Body, nil
}

// Head implements blob.Store.
func (s *Store) Head(ctx context.Context, key string) (blob.Info, error) {
	if !blob.ValidKey(key) {
		return blob.Info{}, &blob.KeyError{Key: key}
	}
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key)})
	if err != nil {
		return blob.Info{}, mapErr(key, err)
	}
	return blob.Info{Key: key, Size: aws.ToInt64(out.ContentLength), Modified: aws.ToTime(out.LastModified)}, nil
}

// Delete implements blob.Store. Deleting a missing object is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if !blob.ValidKey(key) {
		return &blob.KeyError{Key: key}
	}
	if _, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + key)}); err != nil {
		if notFound(err) {
			return nil
		}
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	return nil
}

// List implements blob.Store.
func (s *Store) List(ctx context.Context, prefix string, fn func(blob.Info) error) error {
	if !blob.ValidPrefix(prefix) {
		return &blob.KeyError{Key: prefix}
	}
	p := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix + prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3: list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			if err := fn(blob.Info{Key: strings.TrimPrefix(aws.ToString(o.Key), s.prefix), Size: aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// notFound reports a missing object or bucket.
func notFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	var nsb *types.NoSuchBucket
	if errors.As(err, &nsk) || errors.As(err, &nf) || errors.As(err, &nsb) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}

func mapErr(key string, err error) error {
	if notFound(err) {
		return blob.ErrNotFound
	}
	return fmt.Errorf("s3: %s: %w", key, err)
}

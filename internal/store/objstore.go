// Package store provides storage adapters for NovaGrade, including an
// S3-compatible object store wrapper backed by MinIO.
package store

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config holds the connection settings for an S3-compatible object store
// (MinIO or AWS S3). Credentials are supplied by the caller; no secrets are
// hardcoded in this package.
type Config struct {
	Endpoint  string // host:port of the MinIO/S3 endpoint
	AccessKey string // access key ID
	SecretKey string // secret access key
	UseSSL    bool   // whether to connect over TLS
}

// ObjStore wraps a minio.Client and exposes a small, intention-revealing API
// for bucket and object operations.
type ObjStore struct {
	client *minio.Client
}

// New constructs an ObjStore from the given Config. It returns an error if the
// underlying minio.Client cannot be created.
func New(cfg Config) (*ObjStore, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("store: create minio client: %w", err)
	}
	return &ObjStore{client: client}, nil
}

// EnsureBucket creates the named bucket if it does not already exist. It is
// safe to call repeatedly.
func (s *ObjStore) EnsureBucket(ctx context.Context, name string) error {
	exists, err := s.client.BucketExists(ctx, name)
	if err != nil {
		return fmt.Errorf("store: check bucket %q: %w", name, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, name, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("store: create bucket %q: %w", name, err)
	}
	return nil
}

// Put stores data under bucket/key with the given content type.
func (s *ObjStore) Put(ctx context.Context, bucket, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("store: put %q/%q: %w", bucket, key, err)
	}
	return nil
}

// Get retrieves the full object stored at bucket/key.
func (s *ObjStore) Get(ctx context.Context, bucket, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: get %q/%q: %w", bucket, key, err)
	}
	defer func() { _ = obj.Close() }()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("store: read %q/%q: %w", bucket, key, err)
	}
	return data, nil
}

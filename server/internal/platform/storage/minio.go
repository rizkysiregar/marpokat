// Package storage opens the MinIO (S3 compatible) client used for attachments.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
)

type Store struct {
	Client *minio.Client
	Bucket string
}

// Open creates the client and checks that the bucket exists. The bucket is created by hand (PLAN INF-04),
// never by the app: a typo in MINIO_BUCKET must fail the start, not create a stray bucket.
func Open(ctx context.Context, c config.MinIO) (*Store, error) {
	host, secure, err := c.HostAndTLS()
	if err != nil {
		return nil, err
	}
	mc, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: secure,
		Region: c.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: create client: %w", err)
	}
	s := &Store{Client: mc, Bucket: c.Bucket}
	if err := s.Check(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Check verifies the bucket is reachable. Used at start and by the readiness probe.
func (s *Store) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ok, err := s.Client.BucketExists(ctx, s.Bucket)
	if err != nil {
		return fmt.Errorf("storage: check bucket %q: %w", s.Bucket, err)
	}
	if !ok {
		return fmt.Errorf("storage: bucket %q does not exist", s.Bucket)
	}
	return nil
}

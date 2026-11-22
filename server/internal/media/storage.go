// Package media handles attachments: uploads straight to object storage with presigned links, the thumbnails that are
// made afterwards, and the clean up of uploads nobody used (PLAN M5).
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/minio/minio-go/v7"

	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/platform/storage"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// ErrNoObject: there is nothing in the storage under that key.
var ErrNoObject = errors.New("media: no such object")

const (
	// UploadTTL is how long the link to upload to works.
	UploadTTL = 15 * time.Minute
	// DownloadTTL is how long a link to show or download an attachment works. Messages are rendered with fresh links
	// every time they are read, so a link that expires is only a problem for a screen left open for hours.
	DownloadTTL = time.Hour
)

// Storage is the object storage (MinIO or anything that speaks S3). Files never pass through the API: clients send
// and fetch them with presigned links, the worker reads them to make thumbnails.
type Storage struct {
	client *minio.Client
	bucket string
}

func NewStorage(s *storage.Store) *Storage { return &Storage{client: s.Client, bucket: s.Bucket} }

// PresignPut returns a link to upload one object. The content type is part of the signature: the upload is refused
// unless it comes with the type that was declared (and validated) when the attachment was created.
func (s *Storage) PresignPut(ctx context.Context, key, contentType string) (string, error) {
	u, err := s.client.PresignHeader(ctx, http.MethodPut, s.bucket, key, UploadTTL, url.Values{}, http.Header{"Content-Type": {contentType}})
	if err != nil {
		return "", fmt.Errorf("media: presign upload: %w", err)
	}
	return u.String(), nil
}

// PresignGet returns a link to read one object. attachmentName, when not empty, makes the browser save the file under
// that name instead of showing it.
func (s *Storage) PresignGet(ctx context.Context, key, attachmentName string) (string, error) {
	params := url.Values{}
	if attachmentName != "" {
		params.Set("response-content-disposition", contentDisposition(attachmentName))
	}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, DownloadTTL, params)
	if err != nil {
		return "", fmt.Errorf("media: presign download: %w", err)
	}
	return u.String(), nil
}

// Size is the size of an object, or ErrNoObject.
func (s *Storage) Size(ctx context.Context, key string) (int64, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return 0, ErrNoObject
		}
		return 0, fmt.Errorf("media: stat %s: %w", key, err)
	}
	return info.Size, nil
}

// Open reads an object. The caller closes it.
func (s *Storage) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("media: get %s: %w", key, err)
	}
	if _, err := obj.Stat(); err != nil { // GetObject is lazy: a missing key shows on the first use
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNoObject
		}
		return nil, fmt.Errorf("media: get %s: %w", key, err)
	}
	return obj, nil
}

// Put stores an object.
func (s *Storage) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return fmt.Errorf("media: put %s: %w", key, err)
	}
	return nil
}

// Remove deletes an object; one that is not there is not an error.
func (s *Storage) Remove(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil
		}
		return fmt.Errorf("media: remove %s: %w", key, err)
	}
	return nil
}

// contentDisposition builds "attachment; filename=...; filename*=..." for a file name that came from a user: anything
// that could end the header or the quoted string is dropped from the plain form, and the UTF-8 form is escaped.
func contentDisposition(name string) string {
	var plain strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\' || r == '/' || unicode.IsControl(r) || r > unicode.MaxASCII:
			plain.WriteRune('_')
		default:
			plain.WriteRune(r)
		}
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, plain.String(), url.PathEscape(name))
}

// Signer makes the download links the messages carry (message.AttachmentSigner). Pictures and videos are shown inline;
// everything else, and every type the browser could run, is only ever downloaded.
type Signer struct{ st *Storage }

func NewSigner(st *Storage) *Signer { return &Signer{st: st} }

func (s *Signer) URLs(a store.Attachment) (string, string, error) {
	name := ""
	if message.KindOf(a.Mime) == message.KindFile {
		name = "download"
		if a.FileName != nil && *a.FileName != "" {
			name = *a.FileName
		}
	}
	ctx := context.Background() // signing is local: no request is made
	link, err := s.st.PresignGet(ctx, a.ObjectKey, name)
	if err != nil {
		return "", "", err
	}
	thumb := ""
	if a.ThumbKey != nil && *a.ThumbKey != "" {
		if thumb, err = s.st.PresignGet(ctx, *a.ThumbKey, ""); err != nil {
			return "", "", err
		}
	}
	return link, thumb, nil
}

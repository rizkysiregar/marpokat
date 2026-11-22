package message

import (
	"mime"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// Kinds of attachment. The kind comes from the mime type and decides what happens to an upload: images get a
// thumbnail and a blurhash, videos a poster frame, other files are kept as they are.
const (
	KindImage = "image"
	KindVideo = "video"
	KindFile  = "file"
)

// Attachment statuses (spec openapi: Attachment.status).
const (
	StatusPending    = "pending"    // created, the upload has not been confirmed
	StatusProcessing = "processing" // uploaded, the thumbnail is being made
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

const (
	// MaxAttachmentsPerMessage is the most attachments one message carries.
	MaxAttachmentsPerMessage = 10
	// MaxReactionsPerUser is how many different emoji one user can put on one message.
	MaxReactionsPerUser = 10

	MaxImageSize = 10 << 20
	MaxVideoSize = 50 << 20
	MaxFileSize  = 25 << 20
)

// Only these are treated as pictures and videos: they are what the thumbnail step can handle and what a browser can
// show inline. Anything else (SVG, which can carry script, included) is a file: offered as a download, never inline.
var (
	imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true, "image/heic": true, "image/heif": true}
	videoTypes = map[string]bool{"video/mp4": true, "video/quicktime": true, "video/webm": true, "video/3gpp": true}
	mimeShape  = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)
)

// NormalizeMime lowercases a mime type and drops its parameters ("text/plain; charset=utf-8" is "text/plain"). ok is
// false when it is not a mime type at all.
func NormalizeMime(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if base, _, err := mime.ParseMediaType(s); err == nil {
		s = base
	}
	if len(s) > 127 || !mimeShape.MatchString(s) {
		return "", false
	}
	return s, true
}

// KindOf is the kind of an attachment of this (normalised) mime type.
func KindOf(mimeType string) string {
	switch {
	case imageTypes[mimeType]:
		return KindImage
	case videoTypes[mimeType]:
		return KindVideo
	default:
		return KindFile
	}
}

// MaxSize is the largest upload of a kind, in bytes.
func MaxSize(kind string) int64 {
	switch kind {
	case KindImage:
		return MaxImageSize
	case KindVideo:
		return MaxVideoSize
	default:
		return MaxFileSize
	}
}

// Attachment is the public shape (openapi components/schemas/Attachment).
type Attachment struct {
	ID           uuid.UUID `json:"id"`
	Kind         string    `json:"kind"`
	Mime         string    `json:"mime"`
	Size         int64     `json:"size"`
	FileName     *string   `json:"file_name"`
	Width        *int32    `json:"width"`
	Height       *int32    `json:"height"`
	DurationMS   *int32    `json:"duration_ms"`
	Blurhash     *string   `json:"blurhash"`
	Status       string    `json:"status"`
	URL          *string   `json:"url"`
	ThumbnailURL *string   `json:"thumbnail_url"`
}

// AttachmentSigner makes the download links of an attachment. They are presigned and expire, so a message is rendered
// with fresh ones every time it is read.
type AttachmentSigner interface {
	URLs(a store.Attachment) (url, thumbnailURL string, err error)
}

// AttachmentFrom converts a row. Links are given only once the attachment is ready: before that there is nothing
// worth showing, and a half processed upload should not circulate.
func AttachmentFrom(a store.Attachment, signer AttachmentSigner) Attachment {
	out := Attachment{
		ID: a.ID, Kind: KindOf(a.Mime), Mime: a.Mime, Size: a.Size, FileName: a.FileName,
		Width: a.Width, Height: a.Height, DurationMS: a.DurationMs, Blurhash: a.Blurhash, Status: a.Status,
	}
	if a.Status == StatusReady && signer != nil {
		if url, thumb, err := signer.URLs(a); err == nil {
			out.URL = &url
			if thumb != "" {
				out.ThumbnailURL = &thumb
			}
		}
	}
	return out
}

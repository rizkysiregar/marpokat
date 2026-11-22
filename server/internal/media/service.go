package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/outbox"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// objects is what the service needs of the storage.
type objects interface {
	PresignPut(ctx context.Context, key, contentType string) (string, error)
	Size(ctx context.Context, key string) (int64, error)
	Remove(ctx context.Context, key string) error
}

// Upload is what a client needs to send a file: where to PUT it and with which headers.
type Upload struct {
	Attachment message.Attachment `json:"attachment"`
	UploadURL  string             `json:"upload_url"`
	Headers    map[string]string  `json:"upload_headers"`
	ExpiresAt  time.Time          `json:"expires_at"`
}

// Service creates attachments and confirms their uploads.
type Service struct {
	pool    *pgxpool.Pool
	q       store.Querier
	objects objects
	signer  message.AttachmentSigner
	limiter message.Limiter
	now     func() time.Time
}

func NewService(pool *pgxpool.Pool, st objects, signer message.AttachmentSigner) *Service {
	return &Service{pool: pool, q: store.New(pool), objects: st, signer: signer, now: time.Now}
}

// SetLimiter is called once while wiring.
func (s *Service) SetLimiter(l message.Limiter) { s.limiter = l }

// Starting uploads is cheap for the client and not for the storage, so it is limited: a burst of 20, then one every 2 seconds.
const (
	createBurst     = 20
	createPerSecond = 0.5
)

// Create registers an upload and returns the link to send the file to. The file never passes through the API.
func (s *Service) Create(ctx context.Context, tenantID, userID uuid.UUID, rawMime string, size int64, fileName string) (Upload, error) {
	errs := httpx.FieldErrors{}
	mimeType, ok := message.NormalizeMime(rawMime)
	if !ok {
		errs["mime"] = "must be a media type such as image/jpeg"
	}
	if size < 1 {
		errs["size"] = "must be the size of the file in bytes, at least 1"
	}
	name, err := cleanFileName(fileName)
	if err != nil {
		errs["file_name"] = err.Error()
	}
	if err := errs.OrNil(); err != nil {
		return Upload{}, err
	}
	kind := message.KindOf(mimeType)
	if limit := message.MaxSize(kind); size > limit {
		e := httpx.NewError(413, httpx.CodePayloadTooLarge, "a %s may be at most %d bytes, this one is %d", kind, limit, size)
		e.Details = map[string]any{"kind": kind, "max_bytes": limit}
		return Upload{}, e
	}
	if s.limiter != nil {
		if ok, wait := s.limiter.Allow(ctx, createBurst, createPerSecond, "attach", tenantID.String(), userID.String()); !ok {
			return Upload{}, httpx.ErrRateLimited(wait, "you are starting uploads too fast, try again in %s", wait.Round(time.Millisecond))
		}
	}

	id := uuid.Must(uuid.NewV7())
	key := tenantID.String() + "/" + id.String() // the type travels as metadata of the object, not in its name
	row, err := s.q.InsertAttachment(ctx, store.InsertAttachmentParams{
		ID: id, TenantID: tenantID, UploaderID: userID, ObjectKey: key, Mime: mimeType, Size: size, FileName: name,
	})
	if err != nil {
		return Upload{}, fmt.Errorf("media: create attachment: %w", err)
	}
	link, err := s.objects.PresignPut(ctx, key, mimeType)
	if err != nil {
		return Upload{}, err
	}
	return Upload{
		Attachment: message.AttachmentFrom(row, s.signer),
		UploadURL:  link,
		Headers:    map[string]string{"Content-Type": mimeType},
		ExpiresAt:  s.now().Add(UploadTTL).UTC(),
	}, nil
}

// Complete confirms that the file was uploaded: it must exist with the declared size. Pictures and videos then wait
// for their thumbnail (processing), other files are ready at once. Calling it again changes nothing.
func (s *Service) Complete(ctx context.Context, tenantID, userID, attachmentID uuid.UUID) (message.Attachment, error) {
	var result store.Attachment
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		a, err := q.LockAttachment(ctx, store.LockAttachmentParams{ID: attachmentID, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.UploaderID != userID) {
			return httpx.ErrNotFound("attachment not found")
		}
		if err != nil {
			return err
		}
		result = a
		switch a.Status {
		case message.StatusProcessing, message.StatusReady:
			return nil
		case message.StatusFailed:
			return httpx.ErrConflict("this upload failed, create a new attachment")
		}
		size, err := s.objects.Size(ctx, a.ObjectKey)
		if errors.Is(err, ErrNoObject) {
			return httpx.ErrConflict("no file was uploaded yet: send it to upload_url first")
		}
		if err != nil {
			return err
		}
		if size != a.Size {
			// the declared size is what was checked against the limit: an upload of another size is not accepted
			_ = s.objects.Remove(ctx, a.ObjectKey)
			return errSizeMismatch{declared: a.Size, uploaded: size}
		}
		next := message.StatusReady
		if message.KindOf(a.Mime) != message.KindFile {
			next = message.StatusProcessing
		}
		if err := q.SetAttachmentStatus(ctx, store.SetAttachmentStatusParams{ID: a.ID, TenantID: tenantID, Status: next}); err != nil {
			return err
		}
		result.Status = next
		if next == message.StatusProcessing {
			return outbox.Enqueue(ctx, q, tenantID, outbox.TopicAttachmentUploaded, map[string]any{"attachment_id": a.ID})
		}
		return nil
	})
	var mismatch errSizeMismatch
	switch {
	case errors.As(err, &mismatch):
		// the transaction was rolled back by the error, so the failure is recorded here
		_ = s.q.SetAttachmentStatus(ctx, store.SetAttachmentStatusParams{ID: attachmentID, TenantID: tenantID, Status: message.StatusFailed})
		return message.Attachment{}, httpx.ErrConflict("%s", mismatch.Error())
	case err != nil:
		var apiErr *httpx.Error
		if errors.As(err, &apiErr) {
			return message.Attachment{}, err
		}
		return message.Attachment{}, fmt.Errorf("media: complete: %w", err)
	}
	return message.AttachmentFrom(result, s.signer), nil
}

type errSizeMismatch struct{ declared, uploaded int64 }

func (e errSizeMismatch) Error() string {
	return fmt.Sprintf("the uploaded file is %d bytes but %d were declared: create a new attachment", e.uploaded, e.declared)
}

// cleanFileName keeps the name a person would recognise: no path, no control characters, at most 255 characters.
func cleanFileName(raw string) (*string, error) {
	name := strings.TrimSpace(raw)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return nil, nil
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 255 {
		return nil, errors.New("must be at most 255 characters")
	}
	return &name, nil
}

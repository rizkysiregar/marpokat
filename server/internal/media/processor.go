package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/broker"
	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// Processor makes the thumbnail of an uploaded picture or video, then tells the messages that use it. It is the
// handler of the "media" queue of the worker.
type Processor struct {
	Pool     *pgxpool.Pool
	Objects  *Storage
	FFmpeg   FFmpeg
	Messages *message.Hydrator
	Notifier message.Notifier // may be nil
	Log      *slog.Logger
	Metrics  *metrics.Registry // may be nil
}

func (p *Processor) count(result string) {
	if p.Metrics != nil {
		p.Metrics.MediaProcessed.WithLabelValues(result).Inc()
	}
}

// ThumbnailKey is where the thumbnail of an attachment is kept.
func ThumbnailKey(objectKey string) string { return objectKey + ".thumb.jpg" }

// Handle is the broker.Handler of the media queue. A file that cannot be read is final (the attachment fails);
// trouble with the storage or the database is retried.
func (p *Processor) Handle(ctx context.Context, env broker.Envelope, _ broker.Delivery) error {
	if env.Topic != "attachment.uploaded" {
		return nil
	}
	var pl struct {
		AttachmentID uuid.UUID `json:"attachment_id"`
	}
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return broker.Permanent(fmt.Errorf("undecodable payload: %w", err))
	}
	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return broker.Permanent(fmt.Errorf("bad tenant id %q", env.TenantID))
	}
	q := store.New(p.Pool)
	a, err := q.GetAttachment(ctx, store.GetAttachmentParams{ID: pl.AttachmentID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // cleaned up meanwhile
	}
	if err != nil {
		return fmt.Errorf("load attachment: %w", err)
	}
	if a.Status != message.StatusProcessing { // an event that came twice, or after the work was done
		return nil
	}

	// The upload link works for 15 minutes after /complete verified the size, so the uploader could swap the file for a
	// bigger one in between. What is processed (and what other people are served) must be what was checked.
	size, err := p.Objects.Size(ctx, a.ObjectKey)
	switch {
	case errors.Is(err, ErrNoObject):
		p.Log.Warn("the uploaded object is gone, marking the attachment failed", "attachment_id", a.ID)
		p.count("failed")
		return p.finish(ctx, a, message.StatusFailed, Result{}, "")
	case err != nil:
		return fmt.Errorf("stat attachment %s: %w", a.ID, err)
	case size != a.Size:
		p.Log.Warn("the object changed after it was checked, marking the attachment failed", "attachment_id", a.ID, "declared", a.Size, "found", size)
		p.count("failed")
		_ = p.Objects.Remove(ctx, a.ObjectKey)
		return p.finish(ctx, a, message.StatusFailed, Result{}, "")
	}

	res, err := p.process(ctx, a)
	switch {
	case errors.Is(err, ErrUnreadable):
		p.Log.Warn("attachment cannot be processed, marking it failed", "attachment_id", a.ID, "mime", a.Mime, "error", err)
		p.count("failed")
		return p.finish(ctx, a, message.StatusFailed, Result{}, "")
	case err != nil:
		return fmt.Errorf("process attachment %s: %w", a.ID, err)
	}

	thumbKey := ""
	if len(res.Thumbnail) > 0 {
		thumbKey = ThumbnailKey(a.ObjectKey)
		if err := p.Objects.Put(ctx, thumbKey, bytesReader(res.Thumbnail), int64(len(res.Thumbnail)), "image/jpeg"); err != nil {
			return err
		}
	}
	p.count("ready")
	return p.finish(ctx, a, message.StatusReady, res, thumbKey)
}

// process reads the file and makes what is to be known of it. A video is processed from a file on disk.
func (p *Processor) process(ctx context.Context, a store.Attachment) (Result, error) {
	switch message.KindOf(a.Mime) {
	case message.KindImage:
		obj, err := p.Objects.Open(ctx, a.ObjectKey)
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = obj.Close() }()
		res, err := ProcessImage(obj, message.MaxImageSize)
		if errors.Is(err, ErrUnreadable) && (a.Mime == "image/heic" || a.Mime == "image/heif") {
			// no decoder for HEIC in Go: it is kept as it is, without thumbnail, and shown by apps that can
			return Result{}, nil
		}
		return res, err
	case message.KindVideo:
		if !p.FFmpeg.Enabled() {
			return Result{}, nil // videos are kept as they are
		}
		return p.processVideo(ctx, a)
	default:
		return Result{}, nil
	}
}

func (p *Processor) processVideo(ctx context.Context, a store.Attachment) (Result, error) {
	obj, err := p.Objects.Open(ctx, a.ObjectKey)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = obj.Close() }()
	file, err := os.CreateTemp("", "marpokat-video-*")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := io.Copy(file, io.LimitReader(obj, message.MaxVideoSize+1)); err != nil {
		_ = file.Close()
		return Result{}, fmt.Errorf("download video: %w", err)
	}
	if err := file.Close(); err != nil {
		return Result{}, err
	}
	return p.FFmpeg.ProcessVideo(ctx, file.Name(), message.MaxImageSize*4)
}

// finish records the result and gives every message that shows the attachment a new revision, in one transaction, so
// that a client that syncs finds the change however it missed the event. The event goes out after the commit.
func (p *Processor) finish(ctx context.Context, a store.Attachment, status string, res Result, thumbKey string) error {
	var touched []uuid.UUID
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockAttachment(ctx, store.LockAttachmentParams{ID: a.ID, TenantID: a.TenantID})
		if err != nil {
			return err
		}
		if cur.Status != message.StatusProcessing {
			return nil // somebody else finished it
		}
		var width, height, duration *int32
		if res.Width > 0 {
			w, h := int32(res.Width), int32(res.Height) //nolint:gosec // bounded by maxPixels
			width, height = &w, &h
		}
		duration = res.DurationMS
		var thumb, hash *string
		if thumbKey != "" {
			thumb = &thumbKey
		}
		if res.Blurhash != "" {
			hash = &res.Blurhash
		}
		if err := q.FinishAttachment(ctx, store.FinishAttachmentParams{
			Status: status, Width: width, Height: height, DurationMs: duration, ThumbKey: thumb, Blurhash: hash,
			ID: a.ID, TenantID: a.TenantID,
		}); err != nil {
			return err
		}
		users, err := q.ListMessagesUsingAttachment(ctx, store.ListMessagesUsingAttachmentParams{AttachmentID: a.ID, TenantID: a.TenantID})
		if err != nil {
			return err
		}
		for _, m := range users {
			if _, err := message.Touch(ctx, q, a.TenantID, m.ChannelID, m.ID); err != nil {
				return err
			}
			touched = append(touched, m.ID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("finish attachment %s: %w", a.ID, err)
	}
	if p.Notifier == nil {
		return nil
	}
	for _, id := range touched {
		d, err := store.New(p.Pool).GetMessageByID(ctx, store.GetMessageByIDParams{ID: id, TenantID: a.TenantID})
		if err != nil {
			p.Log.Warn("cannot load a message for its event", "message_id", id, "error", err)
			continue
		}
		msg := message.FromDetail(d)
		if err := p.Messages.HydrateOne(ctx, a.TenantID, a.UploaderID, &msg); err != nil {
			p.Log.Warn("cannot complete a message for its event", "message_id", id, "error", err)
			continue
		}
		p.Notifier.MessageUpdated(ctx, a.TenantID, msg)
	}
	return nil
}

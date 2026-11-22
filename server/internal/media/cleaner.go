package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/store"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Cleaner removes attachments nobody uses: uploads that were never sent in a message (the user changed their mind, the
// upload failed, the app was closed) and those whose message was deleted. They get a day first, since a message may be
// about to be sent with them.
type Cleaner struct {
	q      store.Querier
	object *Storage
	log    *slog.Logger

	Grace time.Duration // default 24 hours
	Every time.Duration // default 10 minutes
	Batch int32         // default 200 per round
}

func NewCleaner(q store.Querier, st *Storage, log *slog.Logger) *Cleaner {
	return &Cleaner{q: q, object: st, log: log.With("subsystem", "media-cleaner"), Grace: 24 * time.Hour, Every: 10 * time.Minute, Batch: 200}
}

// Once removes what is due and returns how many attachments went. An attachment whose files could not be removed is
// left for the next round: the row goes only when the files are gone.
func (c *Cleaner) Once(ctx context.Context) (int, error) {
	removed := 0
	var firstErr error
	for {
		rows, err := c.q.ListUnusedAttachments(ctx, store.ListUnusedAttachmentsParams{CreatedAt: time.Now().Add(-c.Grace), Limit: c.Batch})
		if err != nil {
			return removed, fmt.Errorf("media cleaner: %w", err)
		}
		progressed := 0
		for _, a := range rows {
			if err := c.object.Remove(ctx, a.ObjectKey); err != nil {
				firstErr = err
				continue
			}
			if a.ThumbKey != nil {
				if err := c.object.Remove(ctx, *a.ThumbKey); err != nil {
					firstErr = err
					continue
				}
			}
			if _, err := c.q.DeleteUnusedAttachment(ctx, a.ID); err != nil {
				firstErr = err
				continue
			}
			progressed++
		}
		removed += progressed
		if len(rows) < int(c.Batch) || progressed == 0 {
			return removed, firstErr
		}
	}
}

// Run cleans until ctx ends.
func (c *Cleaner) Run(ctx context.Context) {
	ticker := time.NewTicker(c.Every)
	defer ticker.Stop()
	for {
		if n, err := c.Once(ctx); err != nil {
			c.log.Warn("cleaning failed", "error", err, "removed", n)
		} else if n > 0 {
			c.log.Info("unused attachments removed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

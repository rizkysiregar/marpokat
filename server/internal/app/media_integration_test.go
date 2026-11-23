//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rizkysiregar/marpokat/server/internal/media"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

// ---------------------------------------------------------------- fixtures

func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 120, A: 255})
		}
	}
	return img
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, gradient(w, h), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// jpegWithOrientation is a JPEG that a camera held sideways would produce: the pixels are w x h and an EXIF tag says
// how to turn it (6: a quarter turn clockwise, so a person sees h x w).
func jpegWithOrientation(t *testing.T, w, h int, orientation uint16) []byte {
	t.Helper()
	plain := jpegBytes(t, w, h)
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}                // little endian, first directory at 8
	tiff = append(tiff, 1, 0)                                  // one entry
	tiff = append(tiff, 0x12, 0x01, 3, 0, 1, 0, 0, 0)          // tag 0x0112 (orientation), type SHORT, count 1
	tiff = binary.LittleEndian.AppendUint16(tiff, orientation) // its value
	tiff = append(tiff, 0, 0, 0, 0, 0, 0)                      // padding of the value, no next directory
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xFF, 0xE1}
	segment = binary.BigEndian.AppendUint16(segment, uint16(len(payload)+2)) //nolint:gosec // 40 bytes
	segment = append(segment, payload...)
	return append(append(append([]byte{}, plain[:2]...), segment...), plain[2:]...) // after the SOI marker
}

func pngWithTransparency(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h)) // fully transparent
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sampleVideo(t *testing.T) []byte {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	path := filepath.Join(t.TempDir(), "sample.mp4")
	out, err := exec.Command(ffmpeg, "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-pix_fmt", "yuv420p", path).CombinedOutput()
	if err != nil {
		t.Fatalf("cannot make a sample video: %v: %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// ---------------------------------------------------------------- helpers

type upload struct {
	id      string
	url     string
	headers map[string]string
	body    response
}

func (e *env) startUpload(tt testTenant, ext, mime string, size int, name string) (upload, response) {
	e.t.Helper()
	r := e.asUser(tt, ext, "POST", "/v1/attachments", map[string]any{"mime": mime, "size": size, "file_name": name})
	if r.status != 201 {
		return upload{}, r
	}
	h := map[string]string{}
	for k, v := range r.at("upload_headers").(map[string]any) {
		h[k] = str(v)
	}
	return upload{id: str(r.at("attachment", "id")), url: str(r.at("upload_url")), headers: h, body: r}, r
}

// putObject sends a file to the presigned link like a client would and returns the status the storage answered.
func putObject(t *testing.T, link string, headers map[string]string, data []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, link, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

func getURL(t *testing.T, link string) (int, http.Header, []byte) {
	t.Helper()
	res, err := http.Get(link) //nolint:gosec,noctx // a presigned link of the test storage
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

// uploaded creates, uploads and completes an attachment, and returns its id.
func (e *env) uploaded(tt testTenant, ext, mime, name string, data []byte) string {
	e.t.Helper()
	up, r := e.startUpload(tt, ext, mime, len(data), name)
	if r.status != 201 {
		e.t.Fatalf("create attachment: %d %v", r.status, r.body)
	}
	if got := putObject(e.t, up.url, up.headers, data); got != 200 {
		e.t.Fatalf("upload to the storage: %d", got)
	}
	if c := e.asUser(tt, ext, "POST", "/v1/attachments/"+up.id+"/complete", nil); c.status != 200 {
		e.t.Fatalf("complete: %d %v", c.status, c.body)
	}
	return up.id
}

func (e *env) sendWith(tt testTenant, ext, channel, text string, attachments ...string) response {
	e.t.Helper()
	body := map[string]any{"client_msg_id": fmt.Sprintf("a-%d", time.Now().UnixNano()), "attachment_ids": attachments}
	if text != "" {
		body["text"] = text
	}
	return e.asUser(tt, ext, "POST", "/v1/channels/"+channel+"/messages", body)
}

// waitReady waits until the attachment is ready (or failed) and returns it as the owner sees it.
func (e *env) waitAttachment(tt testTenant, ext, channel, messageID, status string) response {
	e.t.Helper()
	var last response
	waitUntil(e.t, 30*time.Second, "attachment "+status, func() bool {
		last = e.asUser(tt, ext, "GET", "/v1/channels/"+channel+"/messages", nil)
		for _, m := range last.at("messages").([]any) {
			if str(at(m, "id")) == messageID {
				last = response{body: m.(map[string]any)}
				return str(at(m, "attachments", 0, "status")) == status
			}
		}
		return false
	})
	return last
}

// ---------------------------------------------------------------- tests

func TestAttachmentUpload(t *testing.T) {
	e := newRealtimeEnv(t, 1, nil, withMedia())
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	pdf := []byte("%PDF-1.4 a document that is not really one")

	up, r := e.startUpload(tt, "alice", "application/pdf; charset=binary", len(pdf), "../../etc/Report Q3.pdf")
	if r.status != 201 || r.at("attachment", "kind") != "file" || r.at("attachment", "status") != "pending" || r.at("attachment", "mime") != "application/pdf" ||
		r.at("attachment", "url") != nil || r.at("attachment", "file_name") != "Report Q3.pdf" || up.headers["Content-Type"] != "application/pdf" {
		t.Fatalf("create: %d %v", r.status, r.body)
	}
	if exp, err := time.Parse(time.RFC3339Nano, str(r.at("expires_at"))); err != nil || time.Until(exp) < 14*time.Minute || time.Until(exp) > 16*time.Minute {
		t.Fatalf("the upload link must last about 15 minutes: %v", r.at("expires_at"))
	}
	if !strings.HasPrefix(up.url, testdb.MinIO(t).Endpoint) {
		t.Fatalf("the file is sent to the storage, not to the API: %s", up.url)
	}

	// the link is signed for the declared type: another type is refused by the storage itself
	if got := putObject(t, up.url, map[string]string{"Content-Type": "text/html"}, pdf); got != 403 {
		t.Fatalf("an upload with another content type: %d, want 403", got)
	}
	// completing before the file is there
	if c := e.asUser(tt, "alice", "POST", "/v1/attachments/"+up.id+"/complete", nil); c.status != 409 {
		t.Fatalf("complete without a file: %d %v", c.status, c.body)
	}
	if got := putObject(t, up.url, up.headers, pdf); got != 200 {
		t.Fatalf("upload: %d", got)
	}
	if c := e.asUser(tt, "bob", "POST", "/v1/attachments/"+up.id+"/complete", nil); c.status != 404 {
		t.Fatalf("someone else completes: %d", c.status)
	}
	c := e.asUser(tt, "alice", "POST", "/v1/attachments/"+up.id+"/complete", nil)
	if c.status != 200 || c.at("status") != "ready" || c.at("url") == nil || c.at("thumbnail_url") != nil {
		t.Fatalf("a file is ready as soon as it is complete: %d %v", c.status, c.body)
	}
	if again := e.asUser(tt, "alice", "POST", "/v1/attachments/"+up.id+"/complete", nil); again.status != 200 || again.at("status") != "ready" {
		t.Fatalf("completing twice: %d %v", again.status, again.body)
	}

	// a file is only ever downloaded, under the name it came with
	status, header, body := getURL(t, str(c.at("url")))
	if status != 200 || !bytes.Equal(body, pdf) || !strings.HasPrefix(header.Get("Content-Disposition"), "attachment;") ||
		!strings.Contains(header.Get("Content-Disposition"), `filename="Report Q3.pdf"`) {
		t.Fatalf("download: %d %q", status, header.Get("Content-Disposition"))
	}

	// what is refused at the start
	for name, in := range map[string]struct {
		mime string
		size int
	}{"not a mime type": {"photo", 10}, "no size": {"image/jpeg", 0}, "negative size": {"image/jpeg", -5}} {
		if _, r := e.startUpload(tt, "alice", in.mime, in.size, ""); r.status != 400 || r.errCode() != "validation_failed" {
			t.Errorf("%s: %d %v", name, r.status, r.body)
		}
	}
	for name, in := range map[string]struct {
		mime string
		size int
		max  float64
	}{"image over 10 MB": {"image/png", 10<<20 + 1, 10 << 20}, "video over 50 MB": {"video/mp4", 50<<20 + 1, 50 << 20}, "file over 25 MB": {"application/zip", 25<<20 + 1, 25 << 20}} {
		_, r := e.startUpload(tt, "alice", in.mime, in.size, "")
		if r.status != 413 || r.errCode() != "payload_too_large" || r.at("error", "details", "max_bytes") != in.max {
			t.Errorf("%s: %d %v", name, r.status, r.body)
		}
	}
	// SVG can carry script: it is a file to download, never a picture to show
	if _, r := e.startUpload(tt, "alice", "image/svg+xml", 100, "logo.svg"); r.at("attachment", "kind") != "file" {
		t.Errorf("svg: %v", r.body)
	}

	// an upload whose size is not the declared one is refused, and the file is removed
	short, _ := e.startUpload(tt, "alice", "application/pdf", 10, "x.pdf")
	putObject(t, short.url, short.headers, []byte("this is longer than ten bytes"))
	if c := e.asUser(tt, "alice", "POST", "/v1/attachments/"+short.id+"/complete", nil); c.status != 409 || !strings.Contains(str(c.at("error", "message")), "declared") {
		t.Fatalf("size mismatch: %d %v", c.status, c.body)
	}
	if got := countWhere(t, `SELECT count(*) FROM attachments WHERE id = $1 AND status = 'failed'`, mustUUID(t, short.id)); got != 1 {
		t.Fatal("the attachment was not marked failed")
	}
	if _, err := e.files.Size(context.Background(), tt.ID.String()+"/"+short.id); !errors.Is(err, media.ErrNoObject) {
		t.Fatalf("the file that did not match is still in the storage: %v", err)
	}
	if c := e.asUser(tt, "alice", "POST", "/v1/attachments/"+short.id+"/complete", nil); c.status != 409 {
		t.Fatalf("completing a failed upload: %d", c.status)
	}
	if r := e.asUser(tt, "alice", "POST", "/v1/attachments/not-an-id/complete", nil); r.status != 404 {
		t.Fatalf("bad id: %d", r.status)
	}
	if r := e.do("POST", "/v1/attachments", tt.APIKey, "", map[string]any{"mime": "image/png", "size": 1}); r.status != 401 {
		t.Fatalf("without a token: %d", r.status)
	}
}

func TestSendingAttachments(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil, withMedia())
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	// no worker: attachments stay as they are, which is enough for the rules of sending
	photo := e.uploaded(tt, "alice", "image/jpeg", "photo.jpg", jpegBytes(t, 64, 48))   // processing
	doc := e.uploaded(tt, "alice", "application/pdf", "a.pdf", []byte("%PDF-1.4 text")) // ready
	pending, _ := e.startUpload(tt, "alice", "image/png", 100, "p.png")
	broken, _ := e.startUpload(tt, "alice", "application/pdf", 3, "b.pdf")
	putObject(t, broken.url, broken.headers, []byte("longer than three"))
	e.asUser(tt, "alice", "POST", "/v1/attachments/"+broken.id+"/complete", nil)
	bobs := e.uploaded(tt, "bob", "application/pdf", "bob.pdf", []byte("%PDF-1.4 bob"))

	for name, ids := range map[string][]string{
		"not uploaded yet":      {pending.id},
		"failed":                {broken.id},
		"someone else's upload": {bobs},
		"unknown":               {uuid.NewString()},
		"not an id":             {"nope"},
		"the same one twice":    {doc, doc},
		"one good, one pending": {doc, pending.id},
	} {
		if r := e.sendWith(tt, "alice", ch, "x", ids...); r.status != 400 || r.errCode() != "validation_failed" {
			t.Errorf("%s: %d %v", name, r.status, r.body)
		}
	}
	tooMany := make([]string, 11)
	for i := range tooMany {
		tooMany[i] = uuid.NewString()
	}
	if r := e.sendWith(tt, "alice", ch, "x", tooMany...); r.status != 400 {
		t.Errorf("11 attachments: %d", r.status)
	}
	if r := e.asUser(tt, "alice", "GET", "/v1/channels/"+ch+"/messages", nil); count(r.at("messages")) != 0 {
		t.Fatalf("a refused message was stored: %v", r.body)
	}

	// an upload that is still being processed can be sent; a caption is optional
	r := e.sendWith(tt, "alice", ch, "", photo, doc)
	if r.status != 201 || r.at("type") != "media" || r.at("text") != nil || count(r.at("attachments")) != 2 ||
		r.at("attachments", 0, "id") != photo || r.at("attachments", 1, "id") != doc {
		t.Fatalf("send with attachments: %d %v", r.status, r.body)
	}
	if r.at("attachments", 0, "status") != "processing" || r.at("attachments", 0, "url") != nil {
		t.Fatalf("a picture being processed has no link yet: %v", r.at("attachments", 0))
	}
	if r.at("attachments", 1, "status") != "ready" || r.at("attachments", 1, "url") == nil || r.at("attachments", 1, "file_name") != "a.pdf" || num(r.at("attachments", 1, "size")) != 13 {
		t.Fatalf("a file that is ready has its link: %v", r.at("attachments", 1))
	}
	id := str(r.at("id"))
	// the file can be fetched through the link of the message
	if status, _, body := getURL(t, str(r.at("attachments", 1, "url"))); status != 200 || string(body) != "%PDF-1.4 text" {
		t.Fatalf("download through the message: %d %q", status, body)
	}

	// an attachment belongs to one message
	if r := e.sendWith(tt, "alice", ch, "again", doc); r.status != 409 || r.errCode() != "conflict" {
		t.Fatalf("an attachment used twice: %d %v", r.status, r.body)
	}
	// history, sync and the channel list show them, each time with fresh links
	if h := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages", nil); count(h.at("messages", 0, "attachments")) != 2 || h.at("messages", 0, "attachments", 1, "url") == nil {
		t.Fatalf("history: %v", h.body)
	}
	if s := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev=0", nil); count(s.at("messages", 0, "attachments")) != 2 {
		t.Fatalf("sync: %v", s.body)
	}
	if c := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch, nil); count(c.at("last_message", "attachments")) != 2 {
		t.Fatalf("the channel list: %v", c.at("last_message"))
	}
	// sending again with the same client_msg_id returns the message with its attachments
	cmid := "idem-1"
	first := e.asUser(tt, "alice", "POST", "/v1/channels/"+ch+"/messages", map[string]any{"client_msg_id": cmid, "text": "with a file", "attachment_ids": []string{e.uploaded(tt, "alice", "application/zip", "z.zip", []byte("PK"))}})
	again := e.asUser(tt, "alice", "POST", "/v1/channels/"+ch+"/messages", map[string]any{"client_msg_id": cmid, "text": "with a file"})
	if first.status != 201 || again.status != 200 || again.at("id") != first.at("id") || count(again.at("attachments")) != 1 {
		t.Fatalf("idempotent send with an attachment: %d / %d %v", first.status, again.status, again.body)
	}

	// deleting the message lets go of its attachments; they are cleaned up later
	if d := e.asUser(tt, "alice", "DELETE", "/v1/messages/"+id, nil); d.status != 204 {
		t.Fatal(d.status)
	}
	if got := countWhere(t, `SELECT count(*) FROM message_attachments WHERE attachment_id = $1`, mustUUID(t, doc)); got != 0 {
		t.Fatal("the deleted message still holds its attachment")
	}
	if got := countWhere(t, `SELECT count(*) FROM attachments WHERE id = $1`, mustUUID(t, doc)); got != 1 {
		t.Fatal("the attachment itself must stay until the cleaner removes it")
	}

	// the same over the WebSocket
	w := e.connect(0, tt, "alice")
	ws := e.uploaded(tt, "alice", "application/pdf", "ws.pdf", []byte("%PDF-1.4 ws"))
	w.send("message.send", "s1", map[string]any{"channel_id": ch, "client_msg_id": "ws-1", "text": "by websocket", "attachment_ids": []string{ws}})
	if ack := w.next("ack"); at(ack, "data", "message", "attachments", 0, "id") != ws || at(ack, "data", "message", "type") != "media" {
		t.Fatalf("ack with attachment: %v", ack)
	}
	w.send("message.send", "s2", map[string]any{"channel_id": ch, "client_msg_id": "ws-2", "text": "x", "attachment_ids": []string{pending.id}})
	if er := w.next("error"); er["id"] != "s2" || at(er, "data", "code") != "validation_failed" {
		t.Fatalf("websocket send of a pending attachment: %v", er)
	}
}

func TestMediaIsProcessedAndAnnounced(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 2, nil, withMedia())
	g := newFakeGoogle(t)
	w := startMediaWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	bob := e.connect(1, tt, "bob")
	alice := e.connect(0, tt, "alice")

	// the message is sent while the picture is still being processed
	original := jpegBytes(t, 1200, 800)
	photo := e.uploaded(tt, "alice", "image/jpeg", "holiday.jpg", original)
	sent := e.sendWith(tt, "alice", ch, "look!", photo)
	if sent.status != 201 {
		t.Fatal(sent.body)
	}
	id := str(sent.at("id"))
	ev := bob.next("message.new")
	if at(ev, "data", "message", "attachments", 0, "status") != "processing" || at(ev, "data", "message", "attachments", 0, "url") != nil {
		t.Fatalf("message.new while processing: %v", at(ev, "data", "message", "attachments"))
	}
	alice.next("message.new")

	// then every member is told that it is ready, with everything a screen needs
	upd := bob.next("message.updated")
	att := at(upd, "data", "message", "attachments", 0)
	if at(att, "status") != "ready" || num(at(att, "width")) != 1200 || num(at(att, "height")) != 800 || at(att, "url") == nil ||
		at(att, "thumbnail_url") == nil || str(at(att, "blurhash")) == "" || at(upd, "data", "message", "id") != id {
		t.Fatalf("message.updated: %v", att)
	}
	if num(at(upd, "data", "rev_seq")) <= num(at(ev, "data", "rev_seq")) || num(at(upd, "data", "message", "seq")) != num(at(ev, "data", "message", "seq")) {
		t.Fatalf("the change takes a new revision and keeps the position: %v then %v", at(ev, "data", "rev_seq"), at(upd, "data", "rev_seq"))
	}
	alice.next("message.updated")

	// the thumbnail is a small JPEG, the original is untouched
	status, header, body := getURL(t, str(at(att, "thumbnail_url")))
	thumb, err := jpeg.Decode(bytes.NewReader(body))
	if status != 200 || err != nil || header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumbnail: %d %v %s", status, err, header.Get("Content-Type"))
	}
	if b := thumb.Bounds(); b.Dx() != media.ThumbnailSize || b.Dy() < 426 || b.Dy() > 427 { // 800 * 640 / 1200 = 426.67
		t.Fatalf("thumbnail is %dx%d, want 640x426 (or 427)", b.Dx(), b.Dy())
	}
	if status, header, body := getURL(t, str(at(att, "url"))); status != 200 || !bytes.Equal(body, original) || strings.Contains(header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("original: %d %q", status, header.Get("Content-Disposition"))
	}

	// a client that was away finds the change by sync
	s := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev="+fmt.Sprint(num(at(ev, "data", "rev_seq"))), nil)
	if count(s.at("messages")) != 1 || s.at("messages", 0, "attachments", 0, "status") != "ready" || num(s.at("next_rev")) != num(at(upd, "data", "rev_seq")) {
		t.Fatalf("sync after processing: %v", s.body)
	}
	if got := testutil.ToFloat64(w.metrics.MediaProcessed.WithLabelValues("ready")); got != 1 {
		t.Errorf("processed metric = %v", got)
	}

	// a camera held sideways: size and thumbnail are as a person sees the picture
	side := e.uploaded(tt, "alice", "image/jpeg", "side.jpg", jpegWithOrientation(t, 200, 100, 6))
	m := e.sendWith(tt, "alice", ch, "", side)
	got := e.waitAttachment(tt, "alice", ch, str(m.at("id")), "ready")
	if num(got.at("attachments", 0, "width")) != 100 || num(got.at("attachments", 0, "height")) != 200 {
		t.Fatalf("orientation: %vx%v, want 100x200", got.at("attachments", 0, "width"), got.at("attachments", 0, "height"))
	}

	// a transparent picture gets a white thumbnail, not a black one
	clear := e.uploaded(tt, "alice", "image/png", "clear.png", pngWithTransparency(t, 50, 50))
	m = e.sendWith(tt, "alice", ch, "", clear)
	got = e.waitAttachment(tt, "alice", ch, str(m.at("id")), "ready")
	_, _, pngThumb := getURL(t, str(got.at("attachments", 0, "thumbnail_url")))
	if img, err := jpeg.Decode(bytes.NewReader(pngThumb)); err != nil {
		t.Fatal(err)
	} else if r, g, b, _ := img.At(10, 10).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("the transparent area is %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}

	// something that is not a picture fails, and the message says so
	bad := e.uploaded(tt, "alice", "image/jpeg", "broken.jpg", []byte("this is not a jpeg at all"))
	m = e.sendWith(tt, "alice", ch, "", bad)
	got = e.waitAttachment(tt, "alice", ch, str(m.at("id")), "failed")
	if got.at("attachments", 0, "url") != nil || got.at("attachments", 0, "thumbnail_url") != nil {
		t.Fatalf("a failed attachment has no links: %v", got.at("attachments", 0))
	}
	// HEIC cannot be decoded here: it is kept as it is, without thumbnail
	heic := e.uploaded(tt, "alice", "image/heic", "iphone.heic", []byte("ftypheic not decodable"))
	m = e.sendWith(tt, "alice", ch, "", heic)
	got = e.waitAttachment(tt, "alice", ch, str(m.at("id")), "ready")
	if got.at("attachments", 0, "url") == nil || got.at("attachments", 0, "thumbnail_url") != nil || got.at("attachments", 0, "blurhash") != nil {
		t.Fatalf("heic: %v", got.at("attachments", 0))
	}
	// processed before it is sent: ready at once
	early := e.uploaded(tt, "alice", "image/jpeg", "early.jpg", jpegBytes(t, 80, 60))
	waitUntil(t, 20*time.Second, "the picture to be processed", func() bool {
		return countWhere(t, `SELECT count(*) FROM attachments WHERE id = $1 AND status = 'ready'`, mustUUID(t, early)) == 1
	})
	if r := e.sendWith(tt, "alice", ch, "", early); r.at("attachments", 0, "status") != "ready" || r.at("attachments", 0, "thumbnail_url") == nil {
		t.Fatalf("sent after processing: %v", r.at("attachments", 0))
	}
}

func TestVideoGetsAPosterAndADuration(t *testing.T) {
	drainOutbox(t)
	data := sampleVideo(t)
	e := newRealtimeEnv(t, 1, nil, withMedia())
	g := newFakeGoogle(t)
	startMediaWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")

	clip := e.uploaded(tt, "alice", "video/mp4", "clip.mp4", data)
	m := e.sendWith(tt, "alice", ch, "", clip)
	got := e.waitAttachment(tt, "alice", ch, str(m.at("id")), "ready")
	a := got.at("attachments", 0)
	if at(a, "kind") != "video" || num(at(a, "width")) != 320 || num(at(a, "height")) != 240 || at(a, "thumbnail_url") == nil || str(at(a, "blurhash")) == "" {
		t.Fatalf("video: %v", a)
	}
	if d := num(at(a, "duration_ms")); d < 1900 || d > 2200 {
		t.Fatalf("duration %d ms, want about 2000", d)
	}
	if _, _, body := getURL(t, str(at(a, "thumbnail_url"))); len(body) == 0 {
		t.Fatal("empty poster")
	} else if img, err := jpeg.Decode(bytes.NewReader(body)); err != nil || img.Bounds().Dx() != 320 {
		t.Fatalf("poster: %v", err)
	}
}

func TestUnusedAttachmentsAreCleanedUp(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil, withMedia())
	g := newFakeGoogle(t)
	startMediaWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	ctx := context.Background()
	key := func(id string) string { return tt.ID.String() + "/" + id }
	exists := func(k string) bool { _, err := e.files.Size(ctx, k); return err == nil }
	rowExists := func(id string) bool {
		return countWhere(t, `SELECT count(*) FROM attachments WHERE id = $1`, mustUUID(t, id)) == 1
	}

	used := e.uploaded(tt, "alice", "application/pdf", "kept.pdf", []byte("%PDF-1.4 kept"))
	e.sendWith(tt, "alice", ch, "keep it", used)
	abandoned := e.uploaded(tt, "alice", "application/pdf", "abandoned.pdf", []byte("%PDF-1.4 nobody sends this"))
	neverFinished, _ := e.startUpload(tt, "alice", "application/pdf", 9, "x.pdf")
	putObject(t, neverFinished.url, neverFinished.headers, []byte("123456789")) // uploaded, never completed
	neverUploaded, _ := e.startUpload(tt, "alice", "image/png", 99, "y.png")    // not even uploaded
	picture := e.uploaded(tt, "alice", "image/jpeg", "dropped.jpg", jpegBytes(t, 90, 60))
	waitUntil(t, 20*time.Second, "the thumbnail", func() bool {
		return countWhere(t, `SELECT count(*) FROM attachments WHERE id = $1 AND thumb_key IS NOT NULL`, mustUUID(t, picture)) == 1
	})
	deleted := e.uploaded(tt, "alice", "application/pdf", "deleted.pdf", []byte("%PDF-1.4 deleted"))
	gone := e.sendWith(tt, "alice", ch, "to be deleted", deleted)
	e.asUser(tt, "alice", "DELETE", "/v1/messages/"+str(gone.at("id")), nil)

	// nothing is removed while it is young: someone may be about to send it
	cleaner := media.NewCleaner(testStoreQueries(t), e.files, discardLogger())
	young, err := cleaner.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{used, abandoned, neverFinished.id, neverUploaded.id, picture, deleted} {
		if !rowExists(id) {
			t.Fatalf("attachment %s was removed within its first day (cleaner removed %d)", id, young)
		}
	}

	// a negative grace puts the cutoff in the future, so that everything counts as old: the database and this machine
	// (a Docker VM) do not agree on the time to the millisecond, and an upload made a moment ago must be removable here
	cleaner.Grace = -time.Minute
	if _, err := cleaner.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if !rowExists(used) || !exists(key(used)) {
		t.Fatal("an attachment of a message was removed")
	}
	for name, id := range map[string]string{"abandoned": abandoned, "never completed": neverFinished.id, "never uploaded": neverUploaded.id, "dropped picture": picture, "of a deleted message": deleted} {
		if rowExists(id) || exists(key(id)) || exists(key(id)+".thumb.jpg") {
			t.Errorf("%s: the attachment or its files are still there", name)
		}
	}
	// and it is idempotent
	if n, err := cleaner.Once(ctx); err != nil || n != 0 {
		t.Fatalf("second round: %d, %v", n, err)
	}
}

func TestPushSaysWhatKindOfAttachmentWasSent(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil, withMedia())
	g := newFakeGoogle(t)
	startMediaWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "bob-phone")

	photo := e.uploaded(tt, "alice", "image/jpeg", "p.jpg", jpegBytes(t, 40, 30))
	e.sendWith(tt, "alice", ch, "", photo)
	pushes := g.waitDelivered(t, "bob-phone", 1, 20*time.Second)
	if d := pushes[0].Data; d["attachment_kind"] != "image" || d["text"] != "" || d["sender_id"] != "alice" {
		t.Fatalf("push for a photo: %v", d)
	}
	doc := e.uploaded(tt, "alice", "application/pdf", "d.pdf", []byte("%PDF-1.4 d"))
	e.sendWith(tt, "alice", ch, "the report", doc)
	pushes = g.waitDelivered(t, "bob-phone", 2, 20*time.Second)
	if d := pushes[1].Data; d["attachment_kind"] != "file" || d["text"] != "the report" {
		t.Fatalf("push for a file with a caption: %v", d)
	}
}

// The upload link works for 15 minutes after /complete checked the size. An uploader who sends a different (bigger)
// file to the same link in between must not get it processed and served as what was checked.
func TestAFileSwappedAfterTheCheckIsNotProcessed(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 2, nil, withMedia())
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")

	// uploaded and completed, but nobody processes it yet: the worker is not running
	small := jpegBytes(t, 100, 80)
	up, r := e.startUpload(tt, "alice", "image/jpeg", len(small), "photo.jpg")
	if r.status != 201 {
		t.Fatalf("create: %d %v", r.status, r.body)
	}
	if got := putObject(t, up.url, up.headers, small); got != 200 {
		t.Fatalf("upload: %d", got)
	}
	if c := e.asUser(tt, "alice", "POST", "/v1/attachments/"+up.id+"/complete", nil); c.status != 200 {
		t.Fatalf("complete: %d %v", c.status, c.body)
	}
	// now the same link is used again, with another file
	bigger := jpegBytes(t, 1200, 800)
	if got := putObject(t, up.url, up.headers, bigger); got != 200 {
		t.Fatalf("second upload: %d", got)
	}

	g := newFakeGoogle(t)
	w := startMediaWorker(t, g, nil)
	m := e.sendWith(tt, "alice", ch, "", up.id)
	if m.status != 201 {
		t.Fatalf("send: %d %v", m.status, m.body)
	}
	got := e.waitAttachment(tt, "alice", ch, str(m.at("id")), "failed")
	if got.at("attachments", 0, "url") != nil || got.at("attachments", 0, "thumbnail_url") != nil {
		t.Fatalf("a swapped file got links: %v", got.at("attachments", 0))
	}
	if _, err := e.files.Size(context.Background(), tt.ID.String()+"/"+up.id); !errors.Is(err, media.ErrNoObject) {
		t.Fatalf("the swapped object was left in the storage: %v", err)
	}
	if v := testutil.ToFloat64(w.metrics.MediaProcessed.WithLabelValues("failed")); v != 1 {
		t.Errorf("failed metric = %v", v)
	}
}

package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // registers the decoders image.DecodeConfig and imaging.Decode use
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/buckket/go-blurhash"
	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"
)

const (
	// ThumbnailSize is the longest side of a thumbnail, in pixels.
	ThumbnailSize = 640
	thumbQuality  = 80
	// maxPixels refuses pictures that would need gigabytes to decode (a "decompression bomb" is small on disk).
	maxPixels = 64_000_000
)

// ErrUnreadable: the file is not a picture this code can decode. The attachment is marked failed: another try would not help.
var ErrUnreadable = errors.New("media: the file is not a picture that can be decoded")

// Result is what making a thumbnail finds out.
type Result struct {
	Width, Height int
	DurationMS    *int32
	Thumbnail     []byte // JPEG, longest side at most ThumbnailSize
	Blurhash      string
}

// ProcessImage reads a picture and makes its thumbnail and blurhash. Orientation from the camera (EXIF) is applied:
// the size it reports and the thumbnail are what a person sees. Pictures that cannot be decoded (HEIC, which has no
// decoder in Go) are ErrUnreadable.
func ProcessImage(r io.Reader, limit int64) (Result, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return Result{}, fmt.Errorf("media: read picture: %w", err)
	}
	if int64(len(data)) > limit {
		return Result{}, fmt.Errorf("%w: larger than %d bytes", ErrUnreadable, limit)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return Result{}, fmt.Errorf("%w: %dx%d pixels", ErrUnreadable, cfg.Width, cfg.Height)
	}
	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	b := img.Bounds()
	res := Result{Width: b.Dx(), Height: b.Dy()}

	thumb := imaging.Fit(img, ThumbnailSize, ThumbnailSize, imaging.Lanczos)
	// JPEG has no transparency: a transparent PNG would turn black
	flat := imaging.New(thumb.Bounds().Dx(), thumb.Bounds().Dy(), color.White)
	flat = imaging.Overlay(flat, thumb, image.Pt(0, 0), 1)
	var out bytes.Buffer
	if err := imaging.Encode(&out, flat, imaging.JPEG, imaging.JPEGQuality(thumbQuality)); err != nil {
		return Result{}, fmt.Errorf("media: encode thumbnail: %w", err)
	}
	res.Thumbnail = out.Bytes()

	// the blurhash comes from a tiny copy: it describes colours, not detail
	small := imaging.Resize(flat, 32, 0, imaging.Box)
	if res.Blurhash, err = blurhash.Encode(4, 3, small); err != nil {
		return Result{}, fmt.Errorf("media: blurhash: %w", err)
	}
	return res, nil
}

// FFmpeg makes the thumbnail of a video. Without a path (not installed) videos are kept as they are, without poster.
// Only ffmpeg is used, not ffprobe: the duration is in what ffmpeg prints about a file, and a second static binary
// would add 150 MB to an image that the API shares with the worker.
type FFmpeg struct {
	Path string
}

func (f FFmpeg) Enabled() bool { return f.Path != "" }

// ProcessVideo extracts one frame (after a second, or the first frame of a shorter video) and treats it as a picture;
// the duration comes from ffprobe. The video is given as a file because ffmpeg needs to seek in it.
func (f FFmpeg) ProcessVideo(ctx context.Context, videoPath string, imageLimit int64) (Result, error) {
	frame, err := os.CreateTemp("", "marpokat-frame-*.jpg")
	if err != nil {
		return Result{}, err
	}
	framePath := frame.Name()
	_ = frame.Close()
	defer func() { _ = os.Remove(framePath) }()

	var lastErr error
	extracted := false
	for _, seek := range []string{"1", "0"} {
		_ = os.Remove(framePath)
		//nolint:gosec // f.Path is configuration, and the arguments are our own temp files
		out, err := exec.CommandContext(ctx, f.Path, "-y", "-ss", seek, "-i", videoPath, "-frames:v", "1", "-update", "1", "-q:v", "2", framePath).CombinedOutput()
		if st, serr := os.Stat(framePath); err == nil && serr == nil && st.Size() > 0 {
			extracted = true
			break
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		lastErr = fmt.Errorf("ffmpeg -ss %s: %w: %s", seek, err, tail(string(out)))
	}
	if !extracted {
		return Result{}, fmt.Errorf("%w: %w", ErrUnreadable, lastErr)
	}
	file, err := os.Open(framePath) //nolint:gosec // a temp file this function created
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = file.Close() }()
	res, err := ProcessImage(file, imageLimit)
	if err != nil {
		return Result{}, err
	}
	if d, ok := f.duration(ctx, videoPath); ok {
		res.DurationMS = &d
	}
	return res, nil
}

var durationLine = regexp.MustCompile(`Duration:\s*(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)

// duration reads the length of a video from the banner ffmpeg prints when it is given an input and no output. That
// ends in an error (nothing to write), which is expected: only the text matters.
func (f FFmpeg) duration(ctx context.Context, videoPath string) (int32, bool) {
	//nolint:gosec // f.Path is configuration, videoPath a temp file of ours
	out, _ := exec.CommandContext(ctx, f.Path, "-hide_banner", "-i", videoPath).CombinedOutput()
	return parseDuration(string(out))
}

func parseDuration(banner string) (int32, bool) {
	m := durationLine.FindStringSubmatch(banner)
	if m == nil {
		return 0, false // "Duration: N/A": a stream without a known length
	}
	hours, _ := strconv.Atoi(m[1])
	minutes, _ := strconv.Atoi(m[2])
	seconds, err := strconv.ParseFloat(m[3], 64)
	if err != nil {
		return 0, false
	}
	total := float64(hours*3600+minutes*60) + seconds
	if total < 0 || total > math.MaxInt32/1000 {
		return 0, false
	}
	return int32(math.Round(total * 1000)), true
}

// tail keeps error messages short: ffmpeg prints a long banner before the real error.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

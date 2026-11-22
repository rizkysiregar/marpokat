package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
)

func TestContentDispositionCannotBeInjected(t *testing.T) {
	for _, name := range []string{`evil".pdf`, "line\r\nSet-Cookie: x=1", "a\\b", "résumé 日本.pdf"} {
		got := contentDisposition(name)
		if !strings.HasPrefix(got, "attachment; filename=\"") {
			t.Errorf("%q: %q does not start as an attachment", name, got)
		}
		plain := got[len(`attachment; filename="`):strings.Index(got, `"; filename*=`)]
		for _, bad := range []string{`"`, "\r", "\n", `\`} {
			if strings.Contains(plain, bad) {
				t.Errorf("%q: the plain name %q holds %q", name, plain, bad)
			}
		}
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("%q: header contains a line break: %q", name, got)
		}
	}
	if got := contentDisposition("résumé.pdf"); !strings.Contains(got, "filename*=UTF-8''r%C3%A9sum%C3%A9.pdf") {
		t.Errorf("the UTF-8 form is missing: %q", got)
	}
}

func TestCleanFileName(t *testing.T) {
	for in, want := range map[string]string{
		"photo.jpg":             "photo.jpg",
		"  spaced name.pdf  ":   "spaced name.pdf",
		"../../etc/passwd":      "passwd",
		`C:\Users\me\file.docx`: "file.docx",
		"bad\x00name\x07.txt":   "badname.txt",
	} {
		got, err := cleanFileName(in)
		if err != nil || got == nil || *got != want {
			t.Errorf("cleanFileName(%q) = %v, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "dir/"} {
		if got, err := cleanFileName(in); err != nil || got != nil {
			t.Errorf("cleanFileName(%q) = %v, %v; want no name", in, got, err)
		}
	}
	if _, err := cleanFileName(strings.Repeat("x", 256)); err == nil {
		t.Error("a name of 256 characters was accepted")
	}
}

func jpegOf(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func TestProcessImage(t *testing.T) {
	res, err := ProcessImage(bytes.NewReader(jpegOf(1600, 900)), 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 1600 || res.Height != 900 || res.Blurhash == "" || len(res.Thumbnail) == 0 {
		t.Fatalf("result: %dx%d blurhash %q thumbnail %d bytes", res.Width, res.Height, res.Blurhash, len(res.Thumbnail))
	}
	thumb, err := jpeg.Decode(bytes.NewReader(res.Thumbnail))
	if err != nil {
		t.Fatal(err)
	}
	if b := thumb.Bounds(); b.Dx() != ThumbnailSize || b.Dy() != 360 {
		t.Fatalf("thumbnail %dx%d, want 640x360", b.Dx(), b.Dy())
	}
	// a small picture is not blown up
	small, err := ProcessImage(bytes.NewReader(jpegOf(80, 60)), 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := jpeg.DecodeConfig(bytes.NewReader(small.Thumbnail)); b.Width != 80 || b.Height != 60 {
		t.Fatalf("a small picture became %dx%d", b.Width, b.Height)
	}
}

func TestProcessImageRefusesWhatItCannotOrShouldNotDecode(t *testing.T) {
	if _, err := ProcessImage(strings.NewReader("this is not a picture"), 1<<20); !errors.Is(err, ErrUnreadable) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := ProcessImage(bytes.NewReader(jpegOf(100, 100)), 100); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a file over the limit: %v", err)
	}
	if _, err := ProcessImage(bytes.NewReader(pngClaiming(100_000, 100_000)), 1<<20); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a picture that claims 10 billion pixels: %v", err)
	}
}

// pngClaiming is the start of a PNG whose header says it is w x h: a few bytes on disk, many gigabytes decoded. The
// check must happen on the header, before anything is allocated.
func pngClaiming(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2 // 8 bits, RGB
	chunk := append([]byte("IHDR"), ihdr...)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestParseDuration(t *testing.T) {
	banner := "Input #0, mov,mp4, from 'x.mp4':\n  Duration: 00:01:02.50, start: 0.000000, bitrate: 123 kb/s\n"
	if got, ok := parseDuration(banner); !ok || got != 62500 {
		t.Errorf("a minute: %d %v", got, ok)
	}
	if got, ok := parseDuration("  Duration: 01:00:00.00, start"); !ok || got != 3600000 {
		t.Errorf("an hour: %d %v", got, ok)
	}
	if _, ok := parseDuration("  Duration: N/A, bitrate: N/A"); ok {
		t.Error("an unknown duration was reported")
	}
	if _, ok := parseDuration("no banner here"); ok {
		t.Error("text without a duration")
	}
}

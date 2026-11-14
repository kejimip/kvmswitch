package clipimage

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/h265/heic"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func TestImageFormats(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	for ext, enc := range map[string]func(*bytes.Buffer) error{
		".gif":  func(b *bytes.Buffer) error { return gif.Encode(b, src, nil) },
		".bmp":  func(b *bytes.Buffer) error { return bmp.Encode(b, src) },
		".tif":  func(b *bytes.Buffer) error { return tiff.Encode(b, src, nil) },
		".heic": func(b *bytes.Buffer) error { return heic.Encode(b, src, heic.EncodeOptions{Quality: 70}) },
		".avif": func(b *bytes.Buffer) error { return avif.Encode(b, src) },
		".ico": func(b *bytes.Buffer) error {
			var p bytes.Buffer
			if err := png.Encode(&p, src); err != nil {
				return err
			}
			h := make([]byte, 22)
			h[2] = 1
			h[4] = 1
			h[6] = 32
			h[7] = 32
			h[18] = 22
			binary.LittleEndian.PutUint32(h[14:], uint32(p.Len()))
			b.Write(h)
			b.Write(p.Bytes())
			return nil
		},
	} {
		t.Run(ext, func(t *testing.T) {
			var b bytes.Buffer
			if err := enc(&b); err != nil {
				t.Fatal(err)
			}
			im, e := png.Decode(bytes.NewReader(Preview(b.Bytes(), ext)))
			if e != nil || im.Bounds() != src.Bounds() {
				t.Fatal("preview", e)
			}
		})
	}
}

func TestStaticVector(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 24"><rect width="32" height="24" fill="#ff0000"/></svg>`)
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write(svg)
	z.Close()
	for ext, data := range map[string][]byte{".svg": svg, ".svgz": b.Bytes()} {
		im, e := png.Decode(bytes.NewReader(Preview(data, ext)))
		if e != nil {
			t.Fatal(ext, e)
		}
		r, _, _, a := im.At(10, 10).RGBA()
		if im.Bounds().Dx() != 32 || r != 65535 || a != 65535 {
			t.Fatal(ext, "incorrect rendering")
		}
	}
	for _, bad := range []string{`<svg><script/></svg>`, `<svg><image href="http://invalid"/></svg>`, `<svg viewBox="0 0 999999 999999"><rect width="1" height="1"/></svg>`} {
		if Preview([]byte(bad), ".svg") != nil {
			t.Fatal("expected file-only fallback")
		}
	}
}

func TestWebPPreview(t *testing.T) {
	// Lossless 3x2 red WebP produced with Pillow.
	b, e := base64.StdEncoding.DecodeString("UklGRhwAAABXRUJQVlA4TA8AAAAvAkAAAAcQ/Y/+ByKi/wEA")
	if e != nil {
		t.Fatal(e)
	}
	im, e := png.Decode(bytes.NewReader(Preview(b, ".webp")))
	if e != nil {
		t.Fatal(e)
	}
	r, g, blue, a := im.At(1, 1).RGBA()
	if im.Bounds() != image.Rect(0, 0, 3, 2) || r != 65535 || g != 0 || blue != 0 || a != 65535 {
		t.Fatal("incorrect WebP pixels")
	}
}

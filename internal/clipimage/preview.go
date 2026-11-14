package clipimage

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/xml"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/gen2brain/avif"
	_ "github.com/gen2brain/h265/heic"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const MaxBytes = 64 << 20
const maxPixels = 24 << 20
const svgLimit = 2 << 20

func ValidSize(w, h int) bool {
	return w > 0 && h > 0 && w <= maxPixels/h
}

func raster(data []byte) image.Image {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || !ValidSize(cfg.Width, cfg.Height) {
		return nil
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return im
}

// The SVG renderer accepts static shapes and paints only. Never load external
// resources, expand reference trees or execute scripts from clipboard content.
func vector(b []byte) (out image.Image) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	if len(b) > svgLimit {
		return nil
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	depth, n := 0, 0
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil
		}
		switch v := t.(type) {
		case xml.StartElement:
			if n == 0 && v.Name.Local != "svg" {
				return nil
			}
			n++
			depth++
			if n > 8192 || depth > 96 {
				return nil
			}
			switch v.Name.Local {
			case "script", "foreignObject", "image", "use", "filter", "animate", "animateTransform", "set":
				return nil
			}
			for _, a := range v.Attr {
				if a.Name.Local == "href" && !strings.HasPrefix(a.Value, "#") {
					return nil
				}
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return nil
		}
	}
	icon, e := oksvg.ReadIconStream(bytes.NewReader(b), oksvg.StrictErrorMode)
	if e != nil || len(icon.SVGPaths) == 0 {
		return nil
	}
	w, h := icon.ViewBox.W, icon.ViewBox.H
	if !(w > 0 && h > 0 && w <= 8192 && h <= 8192 && math.Ceil(w)*math.Ceil(h) <= maxPixels) {
		return nil
	}
	x, y := int(math.Ceil(w)), int(math.Ceil(h))
	dst := image.NewRGBA(image.Rect(0, 0, x, y))
	icon.SetTarget(0, 0, float64(x), float64(y))
	icon.Draw(rasterx.NewDasher(x, y, rasterx.NewScannerGV(x, y, dst, dst.Bounds())), 1)
	return dst
}

func decode(b []byte, ext string) image.Image {
	switch ext {
	case ".svgz":
		r, e := gzip.NewReader(bytes.NewReader(b))
		if e != nil {
			return nil
		}
		defer r.Close()
		raw, e := io.ReadAll(io.LimitReader(r, svgLimit+1))
		if e != nil {
			return nil
		}
		return vector(raw)
	case ".svg":
		return vector(b)
	case ".ico", ".cur":
		// Modern icons may contain several embedded PNGs. Choose the largest.
		if len(b) < 6 || binary.LittleEndian.Uint16(b) != 0 {
			return nil
		}
		typ := binary.LittleEndian.Uint16(b[2:])
		n := int(binary.LittleEndian.Uint16(b[4:]))
		if (typ != 1 && typ != 2) || n > 256 || len(b) < 6+16*n {
			return nil
		}
		var best []byte
		area := 0
		for i := 0; i < n; i++ {
			v := b[6+16*i:]
			off, size := int64(binary.LittleEndian.Uint32(v[12:])), int64(binary.LittleEndian.Uint32(v[8:]))
			if size == 0 || off+size > int64(len(b)) {
				continue
			}
			p := b[off : off+size]
			c, e := png.DecodeConfig(bytes.NewReader(p))
			if e == nil && c.Width <= 256 && c.Height <= 256 && c.Width*c.Height > area {
				best = p
				area = c.Width * c.Height
			}
		}
		return raster(best)
	default:
		return raster(b)
	}
}

// Preview returns interoperable PNG pixels without changing the source file.
func Preview(b []byte, ext string) []byte {
	if len(b) == 0 || len(b) > MaxBytes {
		return nil
	}
	if bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) {
		c, e := png.DecodeConfig(bytes.NewReader(b))
		if e == nil && ValidSize(c.Width, c.Height) {
			return b
		}
		return nil
	}
	im := decode(b, ext)
	if im == nil {
		return nil
	}
	var out bytes.Buffer
	if png.Encode(&out, im) != nil || out.Len() > MaxBytes {
		return nil
	}
	return out.Bytes()
}

func FilePreview(paths []string) []byte {
	if len(paths) != 1 {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(paths[0]))
	switch ext {
	case ".avif", ".avifs", ".heic", ".heif", ".hif", ".png", ".apng", ".jpg", ".jpeg", ".jfif", ".jpe", ".gif", ".bmp", ".tif", ".tiff", ".webp", ".svg", ".svgz", ".ico", ".cur":
	default:
		return nil
	}
	st, e := os.Lstat(paths[0])
	if e != nil || !st.Mode().IsRegular() || st.Size() > MaxBytes {
		return nil
	}
	f, e := os.Open(paths[0])
	if e != nil {
		return nil
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, st.Size()+1))
	if e != nil || int64(len(b)) != st.Size() {
		return nil
	}
	return Preview(b, ext)
}

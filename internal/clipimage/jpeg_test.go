package clipimage

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestJPEGClipboard(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 6))
	src.Set(3, 4, color.RGBA{R: 200, B: 80, A: 255})
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, src, nil); err != nil {
		t.Fatal(err)
	}
	expected, err := jpeg.Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(Preview(encoded.Bytes(), ".jpeg")))
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds() != expected.Bounds() || color.RGBAModel.Convert(got.At(3, 4)) != color.RGBAModel.Convert(expected.At(3, 4)) {
		t.Fatal("decoded pixels changed")
	}
	p := filepath.Join(t.TempDir(), "测试.JPEG")
	if err = os.WriteFile(p, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if FilePreview([]string{p}) == nil || FilePreview([]string{p, p}) != nil {
		t.Fatal("file selection")
	}
	if err = os.WriteFile(p, []byte("not a JPEG"), 0600); err != nil {
		t.Fatal(err)
	}
	if FilePreview([]string{p}) != nil || Preview(nil, ".jpg") != nil {
		t.Fatal("invalid JPEG accepted")
	}
}

package main

import (
	"bytes"
	"image"
	"image/png"
	"kvmswitch/internal/clipimage"
	"os"
	"path/filepath"
	"testing"
)

func TestSinglePNGFile(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "图片 with spaces.PNG")
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 2, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clipimage.FilePreview([]string{p}), b.Bytes()) {
		t.Fatal("original bytes not preserved")
	}
	for _, paths := range [][]string{nil, {p, p}, {d}, {p + ".missing"}} {
		if clipimage.FilePreview(paths) != nil {
			t.Fatal(paths)
		}
	}
	if err := os.WriteFile(p, []byte("not a PNG"), 0600); err != nil {
		t.Fatal(err)
	}
	if clipimage.FilePreview([]string{p}) != nil {
		t.Fatal("invalid PNG selected")
	}
}

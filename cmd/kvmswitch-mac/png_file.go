package main

import (
	"bytes"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Large files, directories, multiple selections and non-PNGs retain file-only semantics.
func singlePNGFile(paths []string) []byte {
	const limit = 64 << 20
	if len(paths) != 1 || !strings.EqualFold(filepath.Ext(paths[0]), ".png") {
		return nil
	}
	st, err := os.Lstat(paths[0])
	if err != nil || !st.Mode().IsRegular() || st.Size() < 8 || st.Size() > limit {
		return nil
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, st.Size()+1))
	if err != nil || int64(len(data)) != st.Size() {
		return nil
	}
	if _, err := png.DecodeConfig(bytes.NewReader(data)); err != nil {
		return nil
	}
	return data
}

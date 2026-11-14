package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"log"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"kvmswitch/internal/clipimage"
	"kvmswitch/internal/xfer"
)

const (
	cfDIBV5       = 17
	cfDIB         = 8
	cfUnicodeText = 13
	cfHDrop       = 15
	gmemMoveable  = 0x2
)

var (
	cfPNG        = regFormat("PNG")
	cfMimePNG    = regFormat("image/png")
	cfDropEffect = regFormat("Preferred DropEffect")
)

type imageFormat struct {
	id  uintptr
	ext string
}

var extraImages = func() []imageFormat {
	var formats []imageFormat
	for _, f := range [][2]string{
		{"image/svg+xml", ".svg"},
		{"image/jpeg", ".jpg"},
		{"JFIF", ".jpg"},
		{"image/webp", ".webp"},
		{"WebP", ".webp"},
		{"image/gif", ".gif"},
		{"GIF", ".gif"},
		{"image/tiff", ".tiff"},
		{"image/bmp", ".bmp"},
		{"image/avif", ".avif"},
		{"image/heif", ".heif"},
		{"image/heic", ".heic"},
		{"image/x-icon", ".ico"},
	} {
		if id := regFormat(f[0]); id != 0 {
			formats = append(formats, imageFormat{id, f[1]})
		}
	}
	return formats
}()

func regFormat(name string) uintptr {
	r, _, _ := pRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(u16(name))))
	return r
}

func clipboardSeq() uint32 {
	r, _, _ := pGetClipboardSeq.Call()
	return uint32(r)
}

func openClipboard(owner uintptr) bool {
	for i := 0; i < 20; i++ {
		if r, _, _ := pOpenClipboard.Call(owner); r != 0 {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

func hasFormat(f uintptr) bool {
	r, _, _ := pIsClipboardFormatAvail.Call(f)
	return r != 0
}

// globalBytes 复制一个 HGLOBAL 的内容。
func globalBytes(h, limit uintptr) []byte {
	if h == 0 {
		return nil
	}
	size, _, _ := pGlobalSize.Call(h)
	if size == 0 || size > limit {
		return nil
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	defer pGlobalUnlock.Call(h)
	out := make([]byte, size)
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(p)), size))
	return out
}

func getData(f uintptr) []byte {
	h, _, _ := pGetClipboardData.Call(f)
	return globalBytes(h, ^uintptr(0))
}

func getImageData(f uintptr) []byte {
	h, _, _ := pGetClipboardData.Call(f)
	limit := uintptr(clipimage.MaxBytes)
	if f == cfDIB || f == cfDIBV5 {
		limit = (24<<20)*4 + (1 << 20)
	}
	return globalBytes(h, limit)
}

// readClipboard 读取当前剪贴板，优先级：文件 > 图片 > 文本。
func readClipboard() (xfer.Clip, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var clip xfer.Clip
	if !openClipboard(0) {
		return clip, false
	}
	defer pCloseClipboard.Call()

	if hasFormat(cfHDrop) {
		h, _, _ := pGetClipboardData.Call(cfHDrop)
		if h != 0 {
			n, _, _ := pDragQueryFileW.Call(h, 0xFFFFFFFF, 0, 0)
			for i := uintptr(0); i < n; i++ {
				l, _, _ := pDragQueryFileW.Call(h, i, 0, 0)
				buf := make([]uint16, l+1)
				pDragQueryFileW.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), l+1)
				clip.Paths = append(clip.Paths, windows.UTF16ToString(buf))
			}
			if len(clip.Paths) > 0 {
				clip.Kind = xfer.KindFiles
				if b := getData(cfDropEffect); len(b) >= 4 {
					clip.Cut = binary.LittleEndian.Uint32(b)&2 != 0 // DROPEFFECT_MOVE
				}
				return clip, true
			}
		}
	}
	if img, ok := clipboardImage(hasFormat, getImageData); ok {
		return xfer.Clip{Kind: xfer.KindImage, Data: img}, true
	}
	if hasFormat(cfUnicodeText) {
		if b := getData(cfUnicodeText); len(b) >= 2 {
			u := unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2)
			s := windows.UTF16ToString(u)
			if s != "" {
				clip.Kind = xfer.KindText
				clip.Data = []byte(strings.ReplaceAll(s, "\r\n", "\n"))
				return clip, true
			}
		}
	}
	return clip, false
}

// Different applications publish PNG bytes under either registered name.
func clipboardImage(has func(uintptr) bool, get func(uintptr) []byte) ([]byte, bool) {
	for _, f := range []uintptr{cfPNG, cfMimePNG} {
		if f != 0 && has(f) {
			b := get(f)
			if c, err := png.DecodeConfig(bytes.NewReader(b)); err == nil && clipimage.ValidSize(c.Width, c.Height) {
				return b, true
			}
		}
	}
	for _, f := range extraImages {
		if has(f.id) {
			if p := clipimage.Preview(get(f.id), f.ext); p != nil {
				return p, true
			}
		}
	}
	for _, f := range []uintptr{cfDIBV5, cfDIB} {
		if has(f) {
			if b, err := dibToPNG(get(f)); err == nil {
				return b, true
			}
		}
	}
	return nil, false
}

func setData(f uintptr, data []byte) error {
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if h == 0 {
		return errors.New("GlobalAlloc 失败")
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return errors.New("GlobalLock 失败")
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(p)), len(data)), data)
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(f, h); r == 0 {
		pGlobalFree.Call(h)
		return errors.New("SetClipboardData 失败")
	}
	return nil
}

// writeClipboard 把收到的内容写入本机剪贴板，返回写入后的序列号。
func writeClipboard(kind string, data []byte, paths []string) (uint32, error) {
	var filePNG, fileDIB []byte
	if kind == xfer.KindFiles {
		filePNG = clipimage.FilePreview(paths)
		if filePNG != nil {
			fileDIB, _ = pngToDIB(filePNG)
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hwndMain == 0 || !openClipboard(hwndMain) {
		return 0, errors.New("无法打开剪贴板")
	}
	defer pCloseClipboard.Call()
	if r, _, _ := pEmptyClipboard.Call(); r == 0 {
		return 0, errors.New("无法清空剪贴板")
	}
	var err error
	switch kind {
	case xfer.KindText:
		s := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n", "\r\n")
		u, e := windows.UTF16FromString(strings.ReplaceAll(s, "\x00", ""))
		if e != nil {
			return 0, e
		}
		b := unsafe.Slice((*byte)(unsafe.Pointer(&u[0])), len(u)*2)
		err = setData(cfUnicodeText, b)
	case xfer.KindImage:
		err = setData(cfPNG, data)
		if dib, e := pngToDIB(data); e == nil {
			if e2 := setData(cfDIB, dib); e2 == nil {
				err = nil
			}
		}
	case xfer.KindFiles:
		// DROPFILES 头 + 以双 NUL 结尾的宽字符路径列表
		var list []uint16
		for _, p := range paths {
			u, e := windows.UTF16FromString(p)
			if e != nil {
				continue
			}
			list = append(list, u...)
		}
		list = append(list, 0)
		buf := make([]byte, 20+len(list)*2)
		binary.LittleEndian.PutUint32(buf[0:], 20) // pFiles
		binary.LittleEndian.PutUint32(buf[16:], 1) // fWide
		copy(buf[20:], unsafe.Slice((*byte)(unsafe.Pointer(&list[0])), len(list)*2))
		err = setData(cfHDrop, buf)
		if err == nil {
			setData(cfDropEffect, []byte{1, 0, 0, 0}) // DROPEFFECT_COPY
			if filePNG != nil {
				setData(cfPNG, filePNG)
			}
			if len(fileDIB) > 0 {
				if e := setData(cfDIB, fileDIB); e != nil {
					log.Printf("图片文件已就绪，但附加位图失败: %v", e)
				}
			}
		}
	default:
		err = errors.New("未知的剪贴板类型")
	}
	return clipboardSeq(), err
}

// dibToPNG 把 CF_DIB（BITMAPINFOHEADER + 像素）转成 PNG，支持 24/32 位。
func dibToPNG(b []byte) ([]byte, error) {
	if len(b) < 40 {
		return nil, errors.New("DIB 过短")
	}
	hdr := int(binary.LittleEndian.Uint32(b[0:]))
	w := int(int32(binary.LittleEndian.Uint32(b[4:])))
	h := int(int32(binary.LittleEndian.Uint32(b[8:])))
	bpp := int(binary.LittleEndian.Uint16(b[14:]))
	comp := binary.LittleEndian.Uint32(b[16:])
	if hdr < 40 || w <= 0 || h == 0 || (bpp != 24 && bpp != 32) || (comp != 0 && comp != 3) {
		return nil, errors.New("不支持的 DIB 格式")
	}
	topDown := h < 0
	if topDown {
		h = -h
	}
	off := hdr
	if comp == 3 && hdr == 40 {
		off += 12 // 紧跟在头后面的三个颜色掩码
	}
	stride := (w*bpp + 31) / 32 * 4
	if !clipimage.ValidSize(w, h) || w > 32768 || h > 32768 || len(b) < off+stride*h {
		return nil, errors.New("DIB 数据不完整")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	bytesPP := bpp / 8
	anyAlpha := false
	for y := 0; y < h; y++ {
		sy := y
		if !topDown {
			sy = h - 1 - y
		}
		row := b[off+sy*stride:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < w; x++ {
			s := row[x*bytesPP:]
			dst[x*4+0] = s[2]
			dst[x*4+1] = s[1]
			dst[x*4+2] = s[0]
			a := byte(255)
			if bytesPP == 4 {
				a = s[3]
				if a != 0 {
					anyAlpha = true
				}
			}
			dst[x*4+3] = a
		}
	}
	if bytesPP == 4 && !anyAlpha {
		// 很多程序把 alpha 留成全 0，其实表示不透明
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// pngToDIB 生成 32 位自下而上的 CF_DIB。
func pngToDIB(p []byte) ([]byte, error) {
	c, err := png.DecodeConfig(bytes.NewReader(p))
	if err != nil || !clipimage.ValidSize(c.Width, c.Height) {
		return nil, errors.New("PNG 尺寸无效或超限")
	}
	src, err := png.Decode(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	return imageToDIB(src), nil
}

func imageToDIB(src image.Image) []byte {
	r := src.Bounds()
	w, h := r.Dx(), r.Dy()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), src, r.Min, draw.Src)
	out := make([]byte, 40+w*h*4)
	binary.LittleEndian.PutUint32(out[0:], 40)
	binary.LittleEndian.PutUint32(out[4:], uint32(w))
	binary.LittleEndian.PutUint32(out[8:], uint32(h))
	binary.LittleEndian.PutUint16(out[12:], 1)
	binary.LittleEndian.PutUint16(out[14:], 32)
	binary.LittleEndian.PutUint32(out[20:], uint32(w*h*4))
	for y := 0; y < h; y++ {
		srow := img.Pix[(h-1-y)*img.Stride:]
		drow := out[40+y*w*4:]
		for x := 0; x < w; x++ {
			// 不带 alpha 的程序会直接显示颜色，所以把透明处混合到白底
			a := uint32(srow[x*4+3])
			blend := func(c byte) byte { return byte((uint32(c)*a + 255*(255-a)) / 255) }
			drow[x*4+0] = blend(srow[x*4+2])
			drow[x*4+1] = blend(srow[x*4+1])
			drow[x*4+2] = blend(srow[x*4+0])
			drow[x*4+3] = 255
		}
	}
	return out
}

func (*winPlatform) HasClip() bool {
	for _, f := range extraImages {
		if hasFormat(f.id) {
			return true
		}
	}
	return hasFormat(cfHDrop) || hasFormat(cfUnicodeText) || hasFormat(cfPNG) || hasFormat(cfMimePNG) || hasFormat(cfDIBV5) || hasFormat(cfDIB)
}

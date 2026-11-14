package main

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"kvmswitch/internal/clipimage"
	"kvmswitch/internal/xfer"
)

// 通过 Objective-C 运行时访问 NSPasteboard。

const (
	utiText = "public.utf8-plain-text"
	utiPNG  = "public.png"

	utiTIFF    = "public.tiff"
	utiFileURL = "public.file-url"
)

var pasteboardImages = [][2]string{
	{utiPNG, ".png"},
	{"public.avif", ".avif"},
	{"public.heic", ".heic"},
	{"public.heif", ".heif"},
	{"public.jpeg", ".jpg"},
	{"public.svg-image", ".svg"},
	{"public.webp", ".webp"},
	{"org.webmproject.webp", ".webp"},
	{"com.compuserve.gif", ".gif"},
	{"com.microsoft.bmp", ".bmp"},
	{"com.microsoft.ico", ".ico"},
	{utiTIFF, ".tiff"}}

var (
	selMu    sync.Mutex
	selCache = map[string]objc.SEL{}
)

func sel(name string) objc.SEL {
	selMu.Lock()
	defer selMu.Unlock()
	s, ok := selCache[name]
	if !ok {
		s = objc.RegisterName(name)
		selCache[name] = s
	}
	return s
}

func cls(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func loadAppKit() error {
	_, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	return err
}

// withPool 在自动释放池里、固定的系统线程上执行 f。
func withPool(f func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := cls("NSAutoreleasePool").Send(sel("alloc")).Send(sel("init"))
	defer pool.Send(sel("drain"))
	f()
}

func nsString(s string) objc.ID {
	return cls("NSString").Send(sel("stringWithUTF8String:"), strings.ReplaceAll(s, "\x00", ""))
}

func goString(ns objc.ID) string {
	if ns == 0 {
		return ""
	}
	return strings.Clone(objc.Send[string](ns, sel("UTF8String")))
}

func nsDataBytes(d objc.ID) []byte {
	if d == 0 {
		return nil
	}
	n := objc.Send[uint](d, sel("length"))
	if n == 0 || n > clipimage.MaxBytes {
		return nil
	}
	p := objc.Send[unsafe.Pointer](d, sel("bytes"))
	if p == nil {
		return nil
	}
	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(p), n))
	return out
}

func nsData(b []byte) objc.ID {
	if len(b) == 0 {
		return cls("NSData").Send(sel("data"))
	}
	return cls("NSData").Send(sel("dataWithBytes:length:"), unsafe.Pointer(&b[0]), uint(len(b)))
}

func pasteboard() objc.ID { return cls("NSPasteboard").Send(sel("generalPasteboard")) }

func pbChangeCount() int {
	var n int
	withPool(func() { n = objc.Send[int](pasteboard(), sel("changeCount")) })
	return n
}

// readPasteboard 读取剪贴板，优先级：文件 > 图片 > 文本。
func readPasteboard() (clip xfer.Clip, ok bool) {
	withPool(func() {
		pb := pasteboard()
		items := pb.Send(sel("pasteboardItems"))
		if items != 0 {
			n := objc.Send[uint](items, sel("count"))
			for i := uint(0); i < n; i++ {
				item := items.Send(sel("objectAtIndex:"), i)
				s := item.Send(sel("stringForType:"), nsString(utiFileURL))
				if s == 0 {
					continue
				}
				url := cls("NSURL").Send(sel("URLWithString:"), s)
				if url == 0 {
					continue
				}
				if p := goString(url.Send(sel("path"))); p != "" {
					clip.Paths = append(clip.Paths, p)
				}
			}
		}
		if len(clip.Paths) > 0 {
			clip.Kind, ok = xfer.KindFiles, true
			return
		}
		for _, f := range pasteboardImages {
			d := pb.Send(sel("dataForType:"), nsString(f[0]))
			raw := nsDataBytes(d)
			if len(raw) == 0 {
				continue
			}
			b := clipimage.Preview(raw, f[1])
			// AppKit also understands TIFF variants not covered by the portable decoder.
			if b == nil && f[0] == utiTIFF {
				rep := cls("NSBitmapImageRep").Send(sel("imageRepWithData:"), d)
				if rep != 0 && clipimage.ValidSize(objc.Send[int](rep, sel("pixelsWide")), objc.Send[int](rep, sel("pixelsHigh"))) {
					props := cls("NSDictionary").Send(sel("dictionary"))
					b = nsDataBytes(rep.Send(sel("representationUsingType:properties:"), uint(4), props))
				}
			}
			if b != nil {
				clip.Kind, clip.Data, ok = xfer.KindImage, b, true
				return
			}
		}
		if s := goString(pb.Send(sel("stringForType:"), nsString(utiText))); s != "" {
			clip.Kind, clip.Data, ok = xfer.KindText, []byte(s), true
		}
	})
	return
}

// writePasteboard 写入剪贴板，返回写入后的 changeCount。
func writePasteboard(kind string, data []byte, paths []string) (count int, err error) {
	withPool(func() {
		pb := pasteboard()
		pb.Send(sel("clearContents"))
		okv := false
		switch kind {
		case xfer.KindText:
			okv = objc.Send[bool](pb, sel("setString:forType:"), nsString(string(data)), nsString(utiText))
		case xfer.KindImage:
			okv = objc.Send[bool](pb, sel("setData:forType:"), nsData(data), nsString(utiPNG))
		case xfer.KindFiles:
			// One pasteboard item with two representations: Finder consumes the
			// file URL, while image editors can request the image pixels.
			if data := clipimage.FilePreview(paths); data != nil {
				item := cls("NSPasteboardItem").Send(sel("new"))
				if item != 0 {
					defer item.Send(sel("release"))
					url := cls("NSURL").Send(sel("fileURLWithPath:"), nsString(paths[0]))
					if url != 0 && objc.Send[bool](item, sel("setString:forType:"), url.Send(sel("absoluteString")), nsString(utiFileURL)) &&
						objc.Send[bool](item, sel("setData:forType:"), nsData(data), nsString(utiPNG)) {
						okv = objc.Send[bool](pb, sel("writeObjects:"), cls("NSArray").Send(sel("arrayWithObject:"), item))
					}
				}
				if okv {
					break
				}
			}
			arr := cls("NSMutableArray").Send(sel("array"))
			for _, p := range paths {
				if u := cls("NSURL").Send(sel("fileURLWithPath:"), nsString(p)); u != 0 {
					arr.Send(sel("addObject:"), u)
				}
			}
			okv = objc.Send[bool](pb, sel("writeObjects:"), arr)
		}
		if !okv {
			err = errors.New("写入剪贴板失败")
		}
		count = objc.Send[int](pb, sel("changeCount"))
	})
	return
}

// Query advertised formats only; do not materialize large promised pasteboard items.
func (*macPlatform) HasClip() (ok bool) {
	withPool(func() {
		types := pasteboard().Send(sel("types"))
		if types.Send(sel("containsObject:"), nsString(utiFileURL)) != 0 || types.Send(sel("containsObject:"), nsString(utiText)) != 0 {
			ok = true
			return
		}
		for _, f := range pasteboardImages {
			if types.Send(sel("containsObject:"), nsString(f[0])) != 0 {
				ok = true
				return
			}
		}
	})
	return
}

package main

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"kvmswitch/internal/xfer"
)

// 通过 Objective-C 运行时访问 NSPasteboard。

const (
	utiText    = "public.utf8-plain-text"
	utiPNG     = "public.png"
	utiTIFF    = "public.tiff"
	utiFileURL = "public.file-url"
)

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
	p := objc.Send[unsafe.Pointer](d, sel("bytes"))
	if n == 0 || p == nil {
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

// readPasteboard 读取剪贴板，优先级：文件 > 文本 > 图片。
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
		if s := goString(pb.Send(sel("stringForType:"), nsString(utiText))); s != "" {
			clip.Kind, clip.Data, ok = xfer.KindText, []byte(s), true
			return
		}
		if b := nsDataBytes(pb.Send(sel("dataForType:"), nsString(utiPNG))); len(b) > 8 {
			clip.Kind, clip.Data, ok = xfer.KindImage, b, true
			return
		}
		if tiff := pb.Send(sel("dataForType:"), nsString(utiTIFF)); tiff != 0 {
			rep := cls("NSBitmapImageRep").Send(sel("imageRepWithData:"), tiff)
			if rep != 0 {
				props := cls("NSDictionary").Send(sel("dictionary"))
				// NSBitmapImageFileTypePNG = 4
				if b := nsDataBytes(rep.Send(sel("representationUsingType:properties:"), uint(4), props)); len(b) > 8 {
					clip.Kind, clip.Data, ok = xfer.KindImage, b, true
				}
			}
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

// promptAccessibility 让系统弹出“辅助功能”授权提示，并把本程序加入列表。
func promptAccessibility() bool {
	var trusted bool
	withPool(func() {
		yes := cls("NSNumber").Send(sel("numberWithBool:"), true)
		opts := cls("NSDictionary").Send(sel("dictionaryWithObject:forKey:"), yes, nsString("AXTrustedCheckOptionPrompt"))
		trusted = axIsProcessTrustedWithOpts(uintptr(opts))
	})
	return trusted
}

// Query advertised formats only; do not materialize large promised pasteboard items.
func (*macPlatform) HasClip() (ok bool) {
	withPool(func() {
		types := pasteboard().Send(sel("types"))
		for _, k := range []string{utiFileURL, utiText, utiPNG, utiTIFF} {
			if types.Send(sel("containsObject:"), nsString(k)) != 0 {
				ok = true
				break
			}
		}
	})
	return
}

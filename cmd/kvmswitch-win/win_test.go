//go:build windows

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"kvmswitch/internal/xfer"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"kvmswitch/internal/core"
)

// 编译期确认 Windows 平台实现了核心要求的全部能力
var _ core.Platform = (*winPlatform)(nil)

func TestDIBRoundTrip(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 7, 5)) // 奇数宽度，检验行对齐
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			src.SetNRGBA(x, y, color.NRGBA{uint8(x * 30), uint8(y * 50), uint8(x*y + 3), 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, src)
	dib, err := pngToDIB(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	back, err := dibToPNG(dib)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(back))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			if got := color.NRGBAModel.Convert(img.At(x, y)); got != src.NRGBAAt(x, y) {
				t.Fatalf("(%d,%d) = %v，期望 %v", x, y, got, src.NRGBAAt(x, y))
			}
		}
	}
}

// 24 位自下而上的 DIB（截图工具常见格式）
func TestDIB24(t *testing.T) {
	w, h := 3, 2
	stride := 12 // 3*3=9 向上取整到 4 的倍数
	dib := make([]byte, 40+stride*h)
	dib[0] = 40
	dib[4], dib[8], dib[12], dib[14] = byte(w), byte(h), 1, 24
	dib[40+2] = 255        // 底行第 1 个像素：纯红 (BGR)
	dib[40+stride+6] = 255 // 顶行第 3 个像素：纯蓝
	p, err := dibToPNG(dib)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(p))
	if r, _, _, _ := img.At(0, 1).RGBA(); r != 0xFFFF {
		t.Fatal("左下角应为红色")
	}
	if _, _, b, _ := img.At(2, 0).RGBA(); b != 0xFFFF {
		t.Fatal("右上角应为蓝色")
	}
	if _, err := dibToPNG(dib[:50]); err == nil {
		t.Fatal("截断的数据应当报错")
	}
}

// 这些结构体直接传给 Win32，大小必须与系统定义一致（x64）。
func TestStructSizes(t *testing.T) {
	check := func(name string, got, want uintptr) {
		if got != want {
			t.Errorf("%s 大小 = %d，应为 %d", name, got, want)
		}
	}
	check("INPUT(键盘)", unsafe.Sizeof(keyInput{}), 40)
	check("INPUT(鼠标)", unsafe.Sizeof(mouseInput{}), 40)
	check("KBDLLHOOKSTRUCT", unsafe.Sizeof(kbdllhook{}), 24)
	check("MSLLHOOKSTRUCT", unsafe.Sizeof(msllhook{}), 32)
	check("NOTIFYICONDATAW", unsafe.Sizeof(notifyIconData{}), 976)
	check("WNDCLASSEXW", unsafe.Sizeof(wndClassEx{}), 80)
	check("MSG", unsafe.Sizeof(winMsg{}), 48)
	check("PHYSICAL_MONITOR", unsafe.Sizeof(physMonitor{}), 264)
}

func TestRegisteredPNGFormats(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, f := range []uintptr{cfPNG, cfMimePNG} {
		got, ok := clipboardImage(func(k uintptr) bool { return k == f || k == cfUnicodeText }, func(k uintptr) []byte {
			if k == f {
				return pngBytes.Bytes()
			}
			return []byte("caption")
		})
		if !ok || !bytes.Equal(got, pngBytes.Bytes()) {
			t.Fatal("PNG lost", f)
		}
	}
}

// Use a child process with a private Win32 window station: this exercises the
// real clipboard without clearing the developer's interactive clipboard.
func TestClipboardCommitSequence(t *testing.T) {
	const child = "KVM_TEST_PRIVATE_CLIPBOARD"
	if os.Getenv(child) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestClipboardCommitSequence$", "-test.v")
		cmd.Env = append(os.Environ(), child+"=1")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return
	}
	runtime.LockOSThread()
	must := func(v uintptr, _ uintptr, err error) uintptr {
		t.Helper()
		if v == 0 {
			t.Fatal(err)
		}
		return v
	}
	station := must(user32.NewProc("CreateWindowStationW").Call(0, 0, 0xf037f, 0))
	must(user32.NewProc("SetProcessWindowStation").Call(station))
	desktop := must(user32.NewProc("CreateDesktopW").Call(uintptr(unsafe.Pointer(u16("KVMTest"))), 0, 0, 0, 0xf01ff, 0))
	must(user32.NewProc("SetThreadDesktop").Call(desktop))
	hwndMain = must(pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(u16("STATIC"))), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.png")
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []xfer.Clip{{Kind: xfer.KindText, Data: []byte("test")}, {Kind: xfer.KindImage, Data: buf.Bytes()}, {Kind: xfer.KindFiles, Paths: []string{path}}} {
		seq, err := writeClipboard(c.Kind, c.Data, c.Paths)
		if got := clipboardSeq(); err != nil || seq == 0 || got != seq {
			t.Fatalf("%s: write=%d published=%d err=%v", c.Kind, seq, got, err)
		}
	}
}

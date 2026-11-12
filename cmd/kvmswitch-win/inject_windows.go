package main

import (
	"math"
	"sync"
	"unsafe"

	"kvmswitch/internal/core"
	"kvmswitch/internal/proto"
)

// 用 SendInput 把另一台设备发来的键鼠注入本机（键鼠插在 Mac 上、由 Mac 控制 Windows 时使用）。

type keyInput struct {
	Type  uint32
	_     uint32
	Vk    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	_     uint32
	Extra uintptr
	_     [8]byte
}

type mouseInput struct {
	Type      uint32
	_         uint32
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	_         uint32
	Extra     uintptr
}

const (
	keyeventfExtended = 0x1
	keyeventfKeyUp    = 0x2

	mouseeventfMove   = 0x0001
	mouseeventfWheel  = 0x0800
	mouseeventfHWheel = 0x1000
)

func sendKey(vk, scan uint16, up, extended bool) {
	in := keyInput{Type: 1, Vk: vk, Scan: scan}
	if up {
		in.Flags |= keyeventfKeyUp
	}
	if extended {
		in.Flags |= keyeventfExtended
	}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendMouse(dx, dy int32, data uint32, flags uint32) {
	in := mouseInput{Dx: dx, Dy: dy, MouseData: data, Flags: flags}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// 按键编号 -> (按下标志, 抬起标志, mouseData)
var buttonFlags = [5][3]uint32{
	proto.BtnLeft:   {0x0002, 0x0004, 0},
	proto.BtnRight:  {0x0008, 0x0010, 0},
	proto.BtnMiddle: {0x0020, 0x0040, 0},
	proto.BtnX1:     {0x0080, 0x0100, 1},
	proto.BtnX2:     {0x0080, 0x0100, 2},
}

type injector struct {
	swallowPaste bool
	fetch        func() bool
	mu           sync.Mutex
	active       bool
	held         map[uint16]bool // 值为是否扩展键
	buttons      [5]bool
	mouseScale   float64
	scrollScale  float64
	scrollInvert bool
	accX, accY   float64
	wAccY, wAccX float64
}

func newInjector() *injector {
	return &injector{held: map[uint16]bool{}, mouseScale: 1, scrollScale: 1}
}

func (in *injector) tune(cfg *core.Config) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.mouseScale, in.scrollScale, in.scrollInvert = cfg.MouseScale, cfg.ScrollScale, cfg.ScrollInvert
}

func (in *injector) setActive(on bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if on == in.active {
		return
	}
	in.swallowPaste = false
	in.active = on
	if on {
		sendMouse(0, 0, 0, mouseeventfMove) // 唤醒可能已熄灭的屏幕
		return
	}
	// 停止被控：松开所有仍按着的键和鼠标按钮
	for vk, ext := range in.held {
		sendKey(vk, scanOf(vk), true, ext)
	}
	in.held = map[uint16]bool{}
	for b, down := range in.buttons {
		if down {
			sendMouse(0, 0, buttonFlags[b][2], buttonFlags[b][1])
			in.buttons[b] = false
		}
	}
}

func scanOf(vk uint16) uint16 {
	r, _, _ := pMapVirtualKeyW.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC
	return uint16(r)
}

func (in *injector) key(vk uint16, flags byte) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active || vk == 0 || vk > 0xFE {
		return
	}
	down := flags&proto.KeyFlagDown != 0
	ext := flags&proto.KeyFlagExtended != 0
	if vk == 0x56 && in.swallowPaste {
		if !down {
			in.swallowPaste = false
		}
		return
	}
	if down && vk == 0x56 && (hasKey(in.held, 0xA2) || hasKey(in.held, 0xA3) || hasKey(in.held, 0x11)) && !hasKey(in.held, 0xA0) && !hasKey(in.held, 0xA1) && !hasKey(in.held, 0xA4) && !hasKey(in.held, 0xA5) && in.fetch != nil && in.fetch() {
		in.swallowPaste = true
		return
	}
	sendKey(vk, scanOf(vk), !down, ext)
	if down {
		in.held[vk] = ext
	} else {
		delete(in.held, vk)
	}
}

func (in *injector) move(dx, dy int32) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active {
		return
	}
	in.accX += float64(dx) * in.mouseScale
	in.accY += float64(dy) * in.mouseScale
	ix, iy := math.Trunc(in.accX), math.Trunc(in.accY)
	if ix == 0 && iy == 0 {
		return
	}
	in.accX -= ix
	in.accY -= iy
	sendMouse(int32(ix), int32(iy), 0, mouseeventfMove)
}

func (in *injector) button(b int, down bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active || b < 0 || b >= len(in.buttons) {
		return
	}
	in.buttons[b] = down
	f := buttonFlags[b][1]
	if down {
		f = buttonFlags[b][0]
	}
	sendMouse(0, 0, buttonFlags[b][2], f)
}

func (in *injector) wheel(dy, dx int32) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active {
		return
	}
	k := in.scrollScale
	if in.scrollInvert {
		k = -k
	}
	in.wAccY += float64(dy) * k
	in.wAccX += float64(dx) * k
	if v := math.Trunc(in.wAccY); v != 0 {
		in.wAccY -= v
		sendMouse(0, 0, uint32(int32(v)), mouseeventfWheel)
	}
	if v := math.Trunc(in.wAccX); v != 0 {
		in.wAccX -= v
		sendMouse(0, 0, uint32(int32(v)), mouseeventfHWheel)
	}
}

func hasKey(m map[uint16]bool, v uint16) bool { _, ok := m[v]; return ok }

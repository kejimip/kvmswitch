package main

import (
	"math"
	"os/exec"
	"sync"
	"time"

	"github.com/ebitengine/purego"

	"kvmswitch/internal/core"
	"kvmswitch/internal/proto"
)

// 通过 CoreGraphics 的 CGEvent 把收到的键鼠事件注入系统（需要“辅助功能”权限）。

var (
	cgEventSourceCreate         func(state int32) uintptr
	cgEventCreateKeyboardEvent  func(src uintptr, key uint16, down bool) uintptr
	cgEventCreateMouseEvent     func(src uintptr, typ uint32, x, y float64, button uint32) uintptr
	cgEventCreateScrollEvent2   func(src uintptr, units uint32, wheelCount uint32, w1, w2, w3 int32) uintptr
	cgEventPost                 func(tap uint32, ev uintptr)
	cgEventSetFlags             func(ev uintptr, flags uint64)
	cgEventGetFlags             func(ev uintptr) uint64
	cgEventSetType              func(ev uintptr, typ uint32)
	cgEventSetIntegerValueField func(ev uintptr, field uint32, v int64)
	cgMainDisplayID             func() uint32
	cgDisplayPixelsWide         func(d uint32) uintptr
	cgDisplayPixelsHigh         func(d uint32) uintptr
	cfRelease                   func(p uintptr)
	axIsProcessTrusted          func() bool
	cgCanPostEvents             func() bool
	cgRequestPostEvents         func() bool
)

const (
	evLeftDown     = 1
	evLeftUp       = 2
	evRightDown    = 3
	evRightUp      = 4
	evMoved        = 5
	evLeftDragged  = 6
	evRightDragged = 7
	evFlagsChanged = 12
	evOtherDown    = 25
	evOtherUp      = 26
	evOtherDragged = 27

	fieldClickState = 1
	fieldDeltaX     = 4
	fieldDeltaY     = 5
	fieldAutorepeat = 8
)

func loadCG() error {
	cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		return err
	}
	as, err := purego.Dlopen("/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		return err
	}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&cgEventSourceCreate, cg, "CGEventSourceCreate")
	purego.RegisterLibFunc(&cgEventCreateKeyboardEvent, cg, "CGEventCreateKeyboardEvent")
	purego.RegisterLibFunc(&cgEventCreateMouseEvent, cg, "CGEventCreateMouseEvent")
	purego.RegisterLibFunc(&cgEventCreateScrollEvent2, cg, "CGEventCreateScrollWheelEvent2")
	purego.RegisterLibFunc(&cgEventPost, cg, "CGEventPost")
	purego.RegisterLibFunc(&cgEventSetFlags, cg, "CGEventSetFlags")
	purego.RegisterLibFunc(&cgEventGetFlags, cg, "CGEventGetFlags")
	purego.RegisterLibFunc(&cgEventSetType, cg, "CGEventSetType")
	purego.RegisterLibFunc(&cgEventSetIntegerValueField, cg, "CGEventSetIntegerValueField")
	purego.RegisterLibFunc(&cgMainDisplayID, cg, "CGMainDisplayID")
	purego.RegisterLibFunc(&cgDisplayPixelsWide, cg, "CGDisplayPixelsWide")
	purego.RegisterLibFunc(&cgDisplayPixelsHigh, cg, "CGDisplayPixelsHigh")
	purego.RegisterLibFunc(&axSystem, as, "AXUIElementCreateSystemWide")
	purego.RegisterLibFunc(&axGet, as, "AXUIElementCopyAttributeValue")
	purego.RegisterLibFunc(&axTimeout, as, "AXUIElementSetMessagingTimeout")
	purego.RegisterLibFunc(&cfHash, cf, "CFHash")
	purego.RegisterLibFunc(&cgPostPID, cg, "CGEventPostToPid")
	purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&axIsProcessTrusted, as, "AXIsProcessTrusted")
	purego.RegisterLibFunc(&cgCanPostEvents, cg, "CGPreflightPostEventAccess")
	purego.RegisterLibFunc(&cgRequestPostEvents, cg, "CGRequestPostEventAccess")
	return nil
}

// eventTag 写在我们自己注入的事件上（kCGEventSourceUserData），
// 让键鼠采集能认出它们并放行，避免把注入的事件又转发回去。
const (
	eventTag      = 0x4B564D53 // "KVMS"
	fieldUserData = 42
)

type injector struct {
	swallowPaste bool
	mediaHeld    map[uint16]bool
	fetch        func() bool
	mu           sync.Mutex
	src          uintptr
	mods         modMap
	mouseScale   float64
	scrollScale  float64
	scrollInvert bool
	active       bool

	x, y    float64
	w, h    float64
	havePos bool

	flags   uint64
	caps    bool
	modHeld remoteModifiers
	held    map[uint16]bool
	buttons [5]bool

	clickBtn   int
	clickCount int64
	clickTime  time.Time
	clickX     float64
	clickY     float64

	scrollAccX, scrollAccY float64
}

func newInjector() *injector {
	return &injector{mediaHeld: map[uint16]bool{},
		src:     cgEventSourceCreate(1), // kCGEventSourceStateHIDSystemState
		modHeld: remoteModifiers{},
		held:    map[uint16]bool{},
		mods:    defaultMods, mouseScale: 1, scrollScale: 1,
	}
}

func (in *injector) tune(c *core.Config) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.mods = modMap{c.ModCtrl, c.ModAlt, c.ModWin}
	in.mouseScale, in.scrollScale, in.scrollInvert = c.MouseScale, c.ScrollScale, c.ScrollInvert
}

func (in *injector) post(ev uintptr) {
	if ev == 0 {
		return
	}
	cgEventSetIntegerValueField(ev, fieldUserData, eventTag)
	cgEventPost(0, ev) // kCGHIDEventTap
	cfRelease(ev)
}

func (in *injector) applyKeyFlags(ev uintptr, code uint16) {
	cgEventSetFlags(ev, remoteKeyFlags(code, in.flags))
}
func (in *injector) applyPointerFlags(ev uintptr) {
	const nonCoalesced = 0x100
	cgEventSetFlags(ev, cgEventGetFlags(ev)&nonCoalesced|in.flags)
}
func (in *injector) recomputeFlags() { in.flags = in.modHeld.flags(in.caps) }

// enter 开始接受输入。
func (in *injector) enter() {
	in.mu.Lock()
	defer in.mu.Unlock()
	d := cgMainDisplayID()
	in.w, in.h = float64(cgDisplayPixelsWide(d)), float64(cgDisplayPixelsHigh(d))
	if in.w < 1 || in.h < 1 {
		in.w, in.h = 1920, 1080
	}
	if !in.havePos {
		in.x, in.y, in.havePos = in.w/2, in.h/2, true
	}
	in.x = math.Min(math.Max(in.x, 0), in.w-1)
	in.y = math.Min(math.Max(in.y, 0), in.h-1)
	in.active = true
	// 唤醒可能已熄灭的显示器
	if cmd := exec.Command("/usr/bin/caffeinate", "-u", "-t", "2"); cmd.Start() == nil {
		go cmd.Wait()
	}
	in.moveLocked(0, 0) // 让光标显示出来
}

// releaseModifiers 在开始把本机输入转发出去之前，让系统认为物理键盘上按着的修饰键都已松开，
// 否则按住的 Control 等键会在本机“卡住”。
func (in *injector) releaseModifiers() {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, code := range []uint16{59, 62, 58, 61, 55, 54, 56, 60} {
		ev := cgEventCreateKeyboardEvent(in.src, code, false)
		if ev == 0 {
			continue
		}
		cgEventSetType(ev, evFlagsChanged)
		cgEventSetFlags(ev, cgEventGetFlags(ev)&flagCaps)
		in.post(ev)
	}
}

// setActive 开始或停止接受注入。
func (in *injector) setActive(on bool) {
	if on {
		in.mu.Lock()
		was := in.active
		in.mu.Unlock()
		if !was {
			in.enter()
		}
		return
	}
	in.leave()
}

// leave 松开所有仍按着的键和鼠标按钮，停止接受输入。
func (in *injector) leave() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active {
		return
	}
	for nx := range in.mediaHeld {
		postMedia(nx, false, false)
		delete(in.mediaHeld, nx)
	}
	for code := range in.held {
		ev := cgEventCreateKeyboardEvent(in.src, code, false)
		in.applyKeyFlags(ev, code)
		in.post(ev)
	}
	in.held = map[uint16]bool{}
	for vk, code := range in.modHeld {
		delete(in.modHeld, vk)
		in.recomputeFlags()
		ev := cgEventCreateKeyboardEvent(in.src, code, false)
		cgEventSetType(ev, evFlagsChanged)
		in.applyKeyFlags(ev, code)
		in.post(ev)
	}
	for b, down := range in.buttons {
		if down {
			in.buttonLocked(b, false)
		}
	}
	in.swallowPaste = false
	in.active = false
}

// key 注入一次按键。返回 true 表示这是一次“粘贴”组合键（Cmd+V）的按下。
func (in *injector) key(vk uint16, flags byte) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active {
		return false
	}
	down := flags&proto.KeyFlagDown != 0
	if nx, media := mediaKeys[vk]; media {
		postMedia(nx, down, in.mediaHeld[nx] && down)
		if down {
			in.mediaHeld[nx] = true
		} else {
			delete(in.mediaHeld, nx)
		}
		return false
	}
	code, ok := mapKey(vk, flags&proto.KeyFlagExtended != 0, in.mods)
	if !ok {
		return false
	}
	if code == kcCapsLock {
		// 合成的 CapsLock 按键不会真正切换大小写，改为自己维护状态并体现在事件标志里
		if down && !in.held[code] {
			in.caps = !in.caps
			in.recomputeFlags()
		}
		if down {
			in.held[code] = true
		} else {
			delete(in.held, code)
		}
		return false
	}
	if modFlagOf(code) != 0 {
		var changed bool
		code, changed = in.modHeld.update(vk, code, down)
		if !changed {
			return false
		}
		in.recomputeFlags()
		ev := cgEventCreateKeyboardEvent(in.src, code, down)
		cgEventSetType(ev, evFlagsChanged)
		in.applyKeyFlags(ev, code)
		in.post(ev)
		return false
	}
	repeat := down && in.held[code]
	if code == kcV && in.swallowPaste {
		if !down {
			in.swallowPaste = false
		}
		return false
	}
	if code == kcV && down && in.flags&flagCommand != 0 && in.flags&(flagControl|flagShift) == 0 && in.fetch != nil && in.fetch() {
		in.swallowPaste = true
		return false
	}
	ev := cgEventCreateKeyboardEvent(in.src, code, down)
	in.applyKeyFlags(ev, code)
	if repeat {
		cgEventSetIntegerValueField(ev, fieldAutorepeat, 1)
	}
	in.post(ev)
	if down {
		in.held[code] = true
	} else {
		delete(in.held, code)
	}
	return down && !repeat && code == kcV && in.flags&flagCommand != 0 && in.flags&(flagControl|flagShift) == 0
}

func (in *injector) move(dx, dy int32) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.active {
		in.moveLocked(dx, dy)
	}
}

func (in *injector) moveLocked(dx, dy int32) {
	sx, sy := float64(dx)*in.mouseScale, float64(dy)*in.mouseScale
	in.x = math.Min(math.Max(in.x+sx, 0), in.w-1)
	in.y = math.Min(math.Max(in.y+sy, 0), in.h-1)
	typ, btn := uint32(evMoved), uint32(0)
	switch {
	case in.buttons[proto.BtnLeft]:
		typ = evLeftDragged
	case in.buttons[proto.BtnRight]:
		typ, btn = evRightDragged, 1
	case in.buttons[proto.BtnMiddle]:
		typ, btn = evOtherDragged, 2
	case in.buttons[proto.BtnX1]:
		typ, btn = evOtherDragged, 3
	case in.buttons[proto.BtnX2]:
		typ, btn = evOtherDragged, 4
	}
	ev := cgEventCreateMouseEvent(in.src, typ, in.x, in.y, btn)
	if ev == 0 {
		return
	}
	cgEventSetIntegerValueField(ev, fieldDeltaX, int64(math.Round(sx)))
	cgEventSetIntegerValueField(ev, fieldDeltaY, int64(math.Round(sy)))
	in.applyPointerFlags(ev)
	in.post(ev)
}

func (in *injector) button(b int, down bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.active && b >= 0 && b < len(in.buttons) {
		in.buttonLocked(b, down)
	}
}

func (in *injector) buttonLocked(b int, down bool) {
	in.buttons[b] = down
	var typ uint32
	switch b {
	case proto.BtnLeft:
		typ = evLeftUp
		if down {
			typ = evLeftDown
		}
	case proto.BtnRight:
		typ = evRightUp
		if down {
			typ = evRightDown
		}
	default:
		typ = evOtherUp
		if down {
			typ = evOtherDown
		}
	}
	if down {
		// 双击/三击需要自己累计点击次数
		now := time.Now()
		if b == in.clickBtn && now.Sub(in.clickTime) < 450*time.Millisecond &&
			math.Abs(in.x-in.clickX) < 5 && math.Abs(in.y-in.clickY) < 5 {
			in.clickCount++
		} else {
			in.clickCount = 1
		}
		in.clickBtn, in.clickTime, in.clickX, in.clickY = b, now, in.x, in.y
	}
	ev := cgEventCreateMouseEvent(in.src, typ, in.x, in.y, uint32(b))
	if ev == 0 {
		return
	}
	cgEventSetIntegerValueField(ev, fieldClickState, in.clickCount)
	in.applyPointerFlags(ev)
	in.post(ev)
}

func (in *injector) wheel(dy, dx int32) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active {
		return
	}
	// Windows 一格 = 120，换算为 3 行
	k := 3.0 / 120.0 * in.scrollScale
	if in.scrollInvert {
		k = -k
	}
	in.scrollAccY += float64(dy) * k
	in.scrollAccX += -float64(dx) * k // 水平方向两个系统符号相反
	ly, lx := math.Trunc(in.scrollAccY), math.Trunc(in.scrollAccX)
	if ly == 0 && lx == 0 {
		return
	}
	in.scrollAccY -= ly
	in.scrollAccX -= lx
	ev := cgEventCreateScrollEvent2(in.src, 1 /* 按行 */, 2, int32(ly), int32(lx), 0)
	if ev == 0 {
		return
	}
	in.applyPointerFlags(ev)
	in.post(ev)
}

func postMedia(nx uint16, down, repeat bool) {
	withPool(func() {
		phase := uint32(11)
		if down {
			phase = 10
		}
		r := uint32(0)
		if repeat {
			r = 1
		}
		type point struct{ X, Y float64 }
		e := cls("NSEvent").Send(sel("otherEventWithType:location:modifierFlags:timestamp:windowNumber:context:subtype:data1:data2:"),
			uintptr(14), point{}, uintptr(phase<<8), float64(0), int64(0), uintptr(0), int16(8), int64(uint32(nx)<<16|phase<<8|r), int64(-1))
		if ev := uintptr(e.Send(sel("CGEvent"))); ev != 0 {
			cgEventSetIntegerValueField(ev, fieldUserData, eventTag)
			cgEventPost(0, ev)
		}
	})
}

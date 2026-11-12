package main

import (
	"errors"
	"log"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"kvmswitch/internal/core"
	"kvmswitch/internal/proto"
)

// 键鼠采集：用 CGEventTap 监听本机的物理键鼠事件。
// 平时只用来识别热键；处于“转发”状态时吞掉事件并交给核心发往另一台设备。
// 事件源挂在主线程的运行循环上，所以回调在主线程执行，必须尽快返回。

var (
	cgEventTapCreate              func(tap, place, options uint32, mask uint64, cb uintptr, info uintptr) uintptr
	cgEventTapEnable              func(tap uintptr, on bool)
	cgEventGetIntegerValueField   func(ev uintptr, field uint32) int64
	cgAssociateMouseAndCursor     func(connected int32) int32
	cfMachPortCreateRunLoopSource func(alloc, port uintptr, order int) uintptr
	cfRunLoopGetMain              func() uintptr
	cfRunLoopAddSource            func(rl, src, mode uintptr)
	cfRunLoopRun                  func()
	cfRunLoopCommonModes          uintptr
)

const (
	evKeyDown     = 10
	evKeyUp       = 11
	evScrollWheel = 22

	evTapDisabledByTimeout   = 0xFFFFFFFE
	evTapDisabledByUserInput = 0xFFFFFFFF

	fieldMouseButton = 3
	fieldKeycode     = 9
	fieldScrollAxis1 = 11
	fieldScrollAxis2 = 12
	lineToWheelDelta = 40 // Mac 的一行 ≈ Windows 一格(120)的三分之一
)

func loadTap() error {
	cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		return err
	}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&cgEventTapCreate, cg, "CGEventTapCreate")
	purego.RegisterLibFunc(&cgEventTapEnable, cg, "CGEventTapEnable")
	purego.RegisterLibFunc(&cgEventGetIntegerValueField, cg, "CGEventGetIntegerValueField")
	purego.RegisterLibFunc(&cgAssociateMouseAndCursor, cg, "CGAssociateMouseAndMouseCursorPosition")
	purego.RegisterLibFunc(&cfMachPortCreateRunLoopSource, cf, "CFMachPortCreateRunLoopSource")
	purego.RegisterLibFunc(&cfRunLoopGetMain, cf, "CFRunLoopGetMain")
	purego.RegisterLibFunc(&cfRunLoopAddSource, cf, "CFRunLoopAddSource")
	purego.RegisterLibFunc(&cfRunLoopRun, cf, "CFRunLoopRun")
	addr, err := purego.Dlsym(cf, "kCFRunLoopCommonModes")
	if err != nil || addr == 0 {
		return errors.New("找不到 kCFRunLoopCommonModes")
	}
	cfRunLoopCommonModes = *(*uintptr)(unsafe.Pointer(addr))
	return nil
}

type tap struct {
	c       *core.Core
	inj     *injector
	port    uintptr
	forward atomic.Bool
	mods    atomic.Pointer[modMap]

	// 以下只在主线程（回调）里访问
	swallowed map[uint16]bool   // 被热键吞掉按下事件的键，其抬起也要吞掉
	heldMods  map[uint16]uint16 // 当前按住的修饰键：Mac 键码 -> VK
	capsOn    bool
}

func newTap(c *core.Core, inj *injector) *tap {
	t := &tap{c: c, inj: inj, swallowed: map[uint16]bool{}, heldMods: map[uint16]uint16{}}
	m := defaultMods
	t.mods.Store(&m)
	return t
}

func (t *tap) tune(cfg *core.Config) {
	m := modMap{cfg.ModCtrl, cfg.ModAlt, cfg.ModWin}
	t.mods.Store(&m)
}

// start 创建事件监听。没有“辅助功能”权限时创建会失败，此时在后台重试直到用户授权。
func (t *tap) start() {
	cb := purego.NewCallback(t.callback)
	var mask uint64
	for _, typ := range []uint{evLeftDown, evLeftUp, evRightDown, evRightUp, evMoved, evLeftDragged, evRightDragged,
		evKeyDown, evKeyUp, evFlagsChanged, evScrollWheel, evOtherDown, evOtherUp, evOtherDragged} {
		mask |= 1 << typ
	}
	try := func() bool {
		// kCGSessionEventTap(1), kCGHeadInsertEventTap(0), kCGEventTapOptionDefault(0)
		port := cgEventTapCreate(1, 0, 0, mask, cb, 0)
		if port == 0 {
			return false
		}
		t.port = port
		src := cfMachPortCreateRunLoopSource(0, port, 0)
		cfRunLoopAddSource(cfRunLoopGetMain(), src, cfRunLoopCommonModes)
		cgEventTapEnable(port, true)
		log.Printf("键鼠监听已就绪")
		return true
	}
	if try() {
		return
	}
	log.Printf("暂时无法监听键鼠（需要“辅助功能”权限），授权后会自动生效")
	go func() {
		for !try() {
			time.Sleep(2 * time.Second)
		}
	}()
}

// setForward 开始或停止把本机物理输入转发给另一台设备。
func (t *tap) setForward(on bool) {
	if t.forward.Swap(on) == on {
		return
	}
	if on {
		// 转发期间冻结本机光标（位移仍可从事件里读到），并让系统认为修饰键已松开
		cgAssociateMouseAndCursor(0)
		t.inj.releaseModifiers()
	} else {
		cgAssociateMouseAndCursor(1)
	}
}

func (t *tap) hotkeyMods() uint32 {
	var m uint32
	for _, vk := range t.heldMods {
		m |= vkModBit(vk)
	}
	return m
}

func (t *tap) push(ev core.InputEvent) { t.c.PushInput(ev) }

// callback 返回事件本身表示放行，返回 0 表示吞掉。
func (t *tap) callback(proxy uintptr, typ uint32, ev uintptr, info uintptr) uintptr {
	if typ == evTapDisabledByTimeout || typ == evTapDisabledByUserInput {
		if t.port != 0 {
			cgEventTapEnable(t.port, true) // 系统在回调过慢时会停用监听，这里重新启用
		}
		return ev
	}
	if cgEventGetIntegerValueField(ev, fieldUserData) == eventTag {
		return ev // 我们自己注入的事件
	}
	fwd := t.forward.Load()
	mods := *t.mods.Load()

	switch typ {
	case evKeyDown, evKeyUp:
		code := uint16(cgEventGetIntegerValueField(ev, fieldKeycode))
		vk, ext, ok := unmapKey(code, mods)
		if typ == evKeyDown {
			if t.swallowed[code] {
				return 0 // 热键按住不放产生的重复
			}
			repeat := cgEventGetIntegerValueField(ev, fieldAutorepeat) != 0
			if ok && !repeat {
				switch t.c.Keys.OnKeyDown(uint32(vk), t.hotkeyMods(), false) {
				case core.KeySwallow:
					t.swallowed[code] = true
					return 0
				case core.KeyToggle:
					t.swallowed[code] = true
					t.c.Hotkey(false)
					return 0
				case core.KeyEmergency:
					t.swallowed[code] = true
					t.c.Hotkey(true)
					return 0
				}
			}
			if !fwd && !repeat && code == kcV {
				f := cgEventGetFlags(ev)
				if f&flagCommand != 0 && f&(flagControl|flagShift) == 0 {
					t.c.LocalPaste() // 本机键盘上的粘贴：用于完成“剪切”
				}
			}
		} else if t.swallowed[code] {
			delete(t.swallowed, code)
			return 0
		}
		if !fwd {
			return ev
		}
		if ok {
			var f byte
			if typ == evKeyDown {
				f |= proto.KeyFlagDown
			}
			if ext {
				f |= proto.KeyFlagExtended
			}
			t.push(core.InputEvent{Typ: proto.MsgKey, A: int32(vk), C: f})
		}
		return 0

	case evFlagsChanged:
		code := uint16(cgEventGetIntegerValueField(ev, fieldKeycode))
		flags := cgEventGetFlags(ev)
		if code == kcCapsLock {
			on := flags&flagCaps != 0
			if on != t.capsOn {
				t.capsOn = on
				if fwd {
					t.push(core.InputEvent{Typ: proto.MsgKey, A: 0x14, C: proto.KeyFlagDown})
					t.push(core.InputEvent{Typ: proto.MsgKey, A: 0x14})
				}
			}
		} else if bit, isMod := deviceModBit[code]; isMod {
			down := flags&bit != 0
			vk, ext, ok := unmapKey(code, mods)
			if down && ok {
				t.heldMods[code] = vk
			} else {
				delete(t.heldMods, code)
			}
			if fwd && ok {
				var f byte
				if down {
					f |= proto.KeyFlagDown
				}
				if ext {
					f |= proto.KeyFlagExtended
				}
				t.push(core.InputEvent{Typ: proto.MsgKey, A: int32(vk), C: f})
			}
		}
		if fwd {
			return 0
		}
		return ev
	}

	if !fwd {
		return ev
	}
	switch typ {
	case evMoved, evLeftDragged, evRightDragged, evOtherDragged:
		dx := int32(cgEventGetIntegerValueField(ev, fieldDeltaX))
		dy := int32(cgEventGetIntegerValueField(ev, fieldDeltaY))
		if dx != 0 || dy != 0 {
			t.push(core.InputEvent{Typ: proto.MsgMouseMove, A: dx, B: dy})
		}
	case evLeftDown, evLeftUp:
		t.push(core.InputEvent{Typ: proto.MsgMouseButton, A: proto.BtnLeft, C: b2b(typ == evLeftDown)})
	case evRightDown, evRightUp:
		t.push(core.InputEvent{Typ: proto.MsgMouseButton, A: proto.BtnRight, C: b2b(typ == evRightDown)})
	case evOtherDown, evOtherUp:
		btn := int32(proto.BtnMiddle)
		switch cgEventGetIntegerValueField(ev, fieldMouseButton) {
		case 3:
			btn = proto.BtnX1
		case 4:
			btn = proto.BtnX2
		}
		t.push(core.InputEvent{Typ: proto.MsgMouseButton, A: btn, C: b2b(typ == evOtherDown)})
	case evScrollWheel:
		dy := int32(cgEventGetIntegerValueField(ev, fieldScrollAxis1))
		dx := int32(cgEventGetIntegerValueField(ev, fieldScrollAxis2))
		if dy != 0 || dx != 0 {
			// 水平方向两个系统符号相反
			t.push(core.InputEvent{Typ: proto.MsgWheel, A: dy * lineToWheelDelta, B: -dx * lineToWheelDelta})
		}
	}
	return 0
}

func b2b(b bool) byte {
	if b {
		return 1
	}
	return 0
}

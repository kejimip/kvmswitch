package main

import (
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"kvmswitch/internal/core"
	"kvmswitch/internal/proto"
)

// 低级键盘/鼠标钩子：负责识别热键，并在“转发”状态下吞掉本机的物理输入、交给核心发往另一台设备。
// 钩子回调运行在 UI 线程上，必须尽快返回，所以只做判断和入队。
// 由 SendInput 注入的事件（包括另一台设备控制本机时注入的）一律放行，不参与热键判断，也不会被转发。

type kbdllhook struct {
	VkCode, ScanCode, Flags, Time uint32
	ExtraInfo                     uintptr
}

type msllhook struct {
	Pt                     point
	MouseData, Flags, Time uint32
	ExtraInfo              uintptr
}

const (
	whKeyboardLL = 13
	whMouseLL    = 14

	llkhfExtended = 0x01
	llkhfInjected = 0x10
	llkhfUp       = 0x80
	llmhfInjected = 0x01

	wmMouseMove   = 0x200
	wmLButtonDown = 0x201
	wmLButtonUp   = 0x202
	wmRButtonDown = 0x204
	wmRButtonUp   = 0x205
	wmMButtonDown = 0x207
	wmMButtonUp   = 0x208
	wmMouseWheel  = 0x20A
	wmXButtonDown = 0x20B
	wmXButtonUp   = 0x20C
	wmMouseHWheel = 0x20E
)

// 物理修饰键状态位（区分左右）
const (
	bitLCtrl = 1 << iota
	bitRCtrl
	bitLAlt
	bitRAlt
	bitLShift
	bitRShift
	bitLWin
	bitRWin
)

var modVK = [8]uint32{0xA2, 0xA3, 0xA4, 0xA5, 0xA0, 0xA1, 0x5B, 0x5C}

var (
	forward    atomic.Bool   // true = 本机物理输入正在转发给另一台设备
	physMods   atomic.Uint32 // 物理修饰键状态
	swallowed  [256]bool     // 被热键吞掉按下事件的键，其抬起事件也要吞掉
	theCore    *core.Core
	hKbdHook   uintptr
	hMouseHook uintptr
)

func modBit(vk uint32) uint32 {
	for i, v := range modVK {
		if v == vk {
			return 1 << uint(i)
		}
	}
	return 0
}

func collapseMods(m uint32) uint32 {
	var r uint32
	if m&(bitLCtrl|bitRCtrl) != 0 {
		r |= core.ModCtrl
	}
	if m&(bitLAlt|bitRAlt) != 0 {
		r |= core.ModAlt
	}
	if m&(bitLShift|bitRShift) != 0 {
		r |= core.ModShift
	}
	if m&(bitLWin|bitRWin) != 0 {
		r |= core.ModWin
	}
	return r
}

func kbdProc(nCode, wParam, lParam uintptr) uintptr {
	if int32(nCode) == 0 {
		k := (*kbdllhook)(unsafe.Pointer(lParam))
		vk := k.VkCode
		if k.Flags&llkhfInjected == 0 && vk < 256 {
			up := k.Flags&llkhfUp != 0
			bit := modBit(vk)
			if bit != 0 {
				m := physMods.Load()
				if up {
					m &^= bit
				} else {
					m |= bit
				}
				physMods.Store(m)
			}
			if !up {
				if swallowed[vk] {
					return 1 // 热键按住不放产生的重复
				}
				if !forward.Load() {
					theCore.LocalMedia(uint16(vk))
				}
				theCore.Interacted()
				if !forward.Load() && vk == 0x56 && collapseMods(physMods.Load()) == core.ModCtrl && theCore.RequestPaste() {
					swallowed[vk] = true
					return 1
				}
				switch theCore.Keys.OnKeyDown(vk, collapseMods(physMods.Load()), bit != 0) {
				case core.KeySwallow:
					swallowed[vk] = true
					return 1
				case core.KeyToggle:
					swallowed[vk] = true
					theCore.Hotkey(false)
					return 1
				case core.KeyEmergency:
					swallowed[vk] = true
					theCore.Hotkey(true)
					return 1
				}
			} else if swallowed[vk] {
				swallowed[vk] = false
				return 1
			}
			if forward.Load() {
				var f byte
				if !up {
					f |= proto.KeyFlagDown
				}
				if k.Flags&llkhfExtended != 0 {
					f |= proto.KeyFlagExtended
				}
				theCore.PushInput(core.InputEvent{Typ: proto.MsgKey, A: int32(vk), B: int32(k.ScanCode), C: f})
				return 1
			}
		}
	}
	r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}

func mouseProc(nCode, wParam, lParam uintptr) uintptr {
	if int32(nCode) == 0 && wParam != wmMouseMove && (*msllhook)(unsafe.Pointer(lParam)).Flags&llmhfInjected == 0 {
		theCore.Interacted()
	}
	if int32(nCode) == 0 && forward.Load() {
		m := (*msllhook)(unsafe.Pointer(lParam))
		if m.Flags&llmhfInjected == 0 {
			hi := int16(m.MouseData >> 16)
			push := theCore.PushInput
			switch wParam {
			case wmMouseMove:
				// 移动事件被吞掉后光标不会动，所以 事件坐标 - 当前光标 = 位移
				var cur point
				pGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
				dx, dy := m.Pt.X-cur.X, m.Pt.Y-cur.Y
				if dx != 0 || dy != 0 {
					push(core.InputEvent{Typ: proto.MsgMouseMove, A: dx, B: dy})
				}
			case wmLButtonDown, wmLButtonUp:
				push(core.InputEvent{Typ: proto.MsgMouseButton, A: proto.BtnLeft, C: b2b(wParam == wmLButtonDown)})
			case wmRButtonDown, wmRButtonUp:
				push(core.InputEvent{Typ: proto.MsgMouseButton, A: proto.BtnRight, C: b2b(wParam == wmRButtonDown)})
			case wmMButtonDown, wmMButtonUp:
				push(core.InputEvent{Typ: proto.MsgMouseButton, A: proto.BtnMiddle, C: b2b(wParam == wmMButtonDown)})
			case wmXButtonDown, wmXButtonUp:
				btn := int32(proto.BtnX1)
				if hi == 2 {
					btn = proto.BtnX2
				}
				push(core.InputEvent{Typ: proto.MsgMouseButton, A: btn, C: b2b(wParam == wmXButtonDown)})
			case wmMouseWheel:
				push(core.InputEvent{Typ: proto.MsgWheel, A: int32(hi)})
			case wmMouseHWheel:
				push(core.InputEvent{Typ: proto.MsgWheel, B: int32(hi)})
			}
			return 1
		}
	}
	r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}

func b2b(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// installHooks 必须在带消息循环的线程上调用。
func installHooks() error {
	hInst, _, _ := pGetModuleHandleW.Call(0)
	var err error
	hKbdHook, _, err = pSetWindowsHookExW.Call(whKeyboardLL, windows.NewCallback(kbdProc), hInst, 0)
	if hKbdHook == 0 {
		return err
	}
	hMouseHook, _, err = pSetWindowsHookExW.Call(whMouseLL, windows.NewCallback(mouseProc), hInst, 0)
	if hMouseHook == 0 {
		return err
	}
	return nil
}

func removeHooks() {
	if hKbdHook != 0 {
		pUnhookWindowsHookEx.Call(hKbdHook)
	}
	if hMouseHook != 0 {
		pUnhookWindowsHookEx.Call(hMouseHook)
	}
}

// releaseLocalMods 在开始转发前，让 Windows 认为修饰键已松开，
// 否则按住的 Ctrl 等键会在 Windows 上“卡住”。
func releaseLocalMods() {
	m := physMods.Load()
	if m == 0 {
		return
	}
	if m&(bitLAlt|bitRAlt|bitLWin|bitRWin) != 0 {
		// 先插入一个无意义的键，避免单独的 Alt/Win 抬起触发菜单栏或开始菜单
		sendKey(0xE8, 0, false, false)
		sendKey(0xE8, 0, true, false)
	}
	for i, vk := range modVK {
		if m&(1<<uint(i)) != 0 {
			sendKey(uint16(vk), 0, true, false)
		}
	}
}

// parkCursor 把光标停在主屏中央，给位移计算留出余量。
func parkCursor() {
	w, _, _ := pGetSystemMetrics.Call(0)
	h, _, _ := pGetSystemMetrics.Call(1)
	pSetCursorPos.Call(w/2, h/2)
}

// setForward 开始或停止把本机物理输入转发给另一台设备。
func setForward(on bool) {
	if on == forward.Load() {
		return
	}
	if on {
		parkCursor()
		releaseLocalMods()
	}
	forward.Store(on)
}

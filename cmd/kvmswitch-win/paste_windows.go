package main

import "unsafe"

var getForeground = user32.NewProc("GetForegroundWindow")
var getGUIThreadInfo = user32.NewProc("GetGUIThreadInfo")
var getAsyncKeyState = user32.NewProc("GetAsyncKeyState")

func (*winPlatform) PasteApp() uintptr { v, _, _ := getForeground.Call(); return v }
func (*winPlatform) PasteFocus() uintptr {
	var g struct {
		Size, Flags                               uint32
		Active, Focus, Capture, Menu, Move, Caret uintptr
		Rect                                      [4]int32
	}
	g.Size = uint32(unsafe.Sizeof(g))
	r, _, _ := getGUIThreadInfo.Call(0, uintptr(unsafe.Pointer(&g)))
	if r == 0 {
		return 0
	}
	return g.Focus
}
func (p *winPlatform) PasteNow(app, focus uintptr) bool {
	if p.PasteApp() != app || p.PasteFocus() != focus {
		return false
	}
	held := func(v uintptr) bool { r, _, _ := getAsyncKeyState.Call(v); return r&0x8000 != 0 }
	for _, vk := range []uintptr{0x10, 0x12, 0x5b, 0x5c} {
		if held(vk) {
			return false
		}
	}
	keys := make([]keyInput, 0, 4)
	add := func(vk uint16, flags uint32) {
		keys = append(keys, keyInput{Type: 1, Vk: vk, Scan: scanOf(vk), Flags: flags, Extra: kvmKeyTag})
	}
	ctrl := held(0x11)
	if !ctrl {
		add(0x11, 0)
	}
	add(0x56, 0)
	add(0x56, keyeventfKeyUp)
	if !ctrl {
		add(0x11, keyeventfKeyUp)
	}
	r, _, _ := pSendInput.Call(uintptr(len(keys)), uintptr(unsafe.Pointer(&keys[0])), unsafe.Sizeof(keys[0]))
	return int(r) == len(keys)
}

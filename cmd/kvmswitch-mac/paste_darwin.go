package main

var axSystem func() uintptr
var axGet func(uintptr, uintptr, *uintptr) int32
var axTimeout func(uintptr, float32) int32
var cfHash func(uintptr) uintptr
var cgPostPID func(int32, uintptr)

func (*macPlatform) PasteApp() (pid uintptr) {
	withPool(func() {
		pid = uintptr(cls("NSWorkspace").Send(sel("sharedWorkspace")).Send(sel("frontmostApplication")).Send(sel("processIdentifier")))
	})
	return
}
func (*macPlatform) PasteFocus() (focus uintptr) {
	withPool(func() {
		sys := axSystem()
		if sys == 0 {
			return
		}
		defer cfRelease(sys)
		axTimeout(sys, 0.075)
		var elem uintptr
		if axGet(sys, uintptr(nsString("AXFocusedUIElement")), &elem) == 0 && elem != 0 {
			defer cfRelease(elem)
			focus = cfHash(elem)
		}
	})
	return
}
func (p *macPlatform) PasteNow(app, focus uintptr) bool {
	if p.PasteApp() != app || p.PasteFocus() != focus {
		return false
	}
	for _, down := range []bool{true, false} {
		ev := cgEventCreateKeyboardEvent(0, kcV, down)
		if ev == 0 {
			return false
		}
		cgEventSetFlags(ev, flagCommand)
		cgEventSetIntegerValueField(ev, fieldUserData, eventTag)
		cgPostPID(int32(app), ev)
		cfRelease(ev)
	}
	return true
}

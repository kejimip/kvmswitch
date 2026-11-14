package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"kvmswitch/internal/core"
	"kvmswitch/internal/panel"
)

// 托盘图标、右键菜单、面板窗口，以及承载键鼠钩子的消息循环。

const (
	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmApp       = 0x8000
	wmTray      = wmApp + 1
	wmRefresh   = wmApp + 2
	wmBalloon   = wmApp + 3
	wmOpenPanel = wmApp + 4
	wmAwake     = wmApp + 5

	wndClassName  = "KVMSwitchWindow"
	panelWinTitle = "KVMSwitch 操作面板"

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	nifMsg    = 1
	nifIcon   = 2
	nifTip    = 4
	nifInfo   = 0x10

	mfString    = 0x0
	mfGrayed    = 0x1
	mfChecked   = 0x8
	mfSeparator = 0x800

	idToggle    = 101
	idPanel     = 102
	idAutostart = 103
	idQuit      = 104
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

type balloon struct{ title, text string }

var (
	hwndMain         uintptr
	instance         string // 调试用的实例名后缀，正常为空
	uiMu             sync.Mutex
	uiCur            core.State
	balloons         []balloon
	iconSelf         uintptr
	iconPeer         uintptr
	iconOff          uintptr
	msgTaskbarCreate uint32
	keepAwake        atomic.Bool
	panelSrv         *panel.Server
	panelLaunch      atomic.Bool
	panelPID         atomic.Uint32
	pFindWindowExW   = user32.NewProc("FindWindowExW")
)

func uiRefresh(st core.State) {
	uiMu.Lock()
	uiCur = st
	uiMu.Unlock()
	if hwndMain != 0 {
		pPostMessageW.Call(hwndMain, wmRefresh, 0, 0)
	}
}

func notify(title, text string) {
	uiMu.Lock()
	balloons = append(balloons, balloon{title, text})
	uiMu.Unlock()
	if hwndMain != 0 {
		pPostMessageW.Call(hwndMain, wmBalloon, 0, 0)
	}
}

// makeIcon 生成一个纯色圆形图标。
func makeIcon(r, g, b byte) uintptr {
	const n = 32
	xor := make([]byte, n*n*4)
	and := make([]byte, n*n/8)
	c := float64(n-1) / 2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-c, float64(y)-c
			d := dx*dx + dy*dy
			var a float64
			switch {
			case d <= 13.5*13.5:
				a = 1
			case d <= 15*15:
				a = 0.5
			}
			i := (y*n + x) * 4
			// 预乘 alpha 的 BGRA
			xor[i+0] = byte(float64(b) * a)
			xor[i+1] = byte(float64(g) * a)
			xor[i+2] = byte(float64(r) * a)
			xor[i+3] = byte(255 * a)
		}
	}
	hInst, _, _ := pGetModuleHandleW.Call(0)
	h, _, _ := pCreateIcon.Call(hInst, n, n, 1, 32, uintptr(unsafe.Pointer(&and[0])), uintptr(unsafe.Pointer(&xor[0])))
	return h
}

func copyU16(dst []uint16, s string) {
	u, _ := windows.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func peerLabel(st core.State) string {
	if st.PeerName != "" {
		return st.PeerName
	}
	return "另一台设备"
}

func trayData() *notifyIconData {
	uiMu.Lock()
	st := uiCur
	uiMu.Unlock()
	nid := &notifyIconData{HWnd: hwndMain, UID: 1, UFlags: nifMsg | nifIcon | nifTip, UCallbackMessage: wmTray}
	nid.CbSize = uint32(unsafe.Sizeof(*nid))
	tip := "KVMSwitch - 另一台设备未连接"
	nid.HIcon = iconOff
	if st.Connected {
		if st.Active == core.ActivePeer {
			tip = "KVMSwitch - 键鼠在 " + peerLabel(st)
			nid.HIcon = iconPeer
		} else {
			tip = "KVMSwitch - 键鼠在本机（" + peerLabel(st) + " 已连接）"
			nid.HIcon = iconSelf
		}
	}
	copyU16(nid.SzTip[:], tip)
	return nid
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("KVMSwitch")
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		return k.DeleteValue("KVMSwitch")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue("KVMSwitch", `"`+exe+`" --background`)
}

// Explorer may expose a TabProxyWindow with our exact title even when the
// WebView is closed. Only activate a real window owned by our live child.
func ownedPanelWindow(pid uint32) uintptr {
	if pid == 0 {
		return 0
	}
	class, title := u16("webview"), u16(panelWinTitle)
	var prev uintptr
	for {
		h, _, _ := pFindWindowExW.Call(0, prev, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
		if h == 0 {
			return 0
		}
		var owner uint32
		if _, err := windows.GetWindowThreadProcessId(windows.HWND(h), &owner); err == nil && owner == pid {
			return h
		}
		prev = h
	}
}

// openPanel 显示操作面板窗口；已打开则把它带到前台。tab 为要定位到的页面，可为空。
func openPanel(tab string) {
	if panelSrv == nil {
		return
	}
	if h := ownedPanelWindow(panelPID.Load()); h != 0 {
		if r, _, _ := pIsIconic.Call(h); r != 0 {
			pShowWindow.Call(h, 9) // SW_RESTORE
		}
		pSetForegroundWindow.Call(h)
		return
	}
	if !panelLaunch.CompareAndSwap(false, true) {
		return
	}
	defer panelLaunch.Store(false)
	exe, err := os.Executable()
	if err == nil {
		cmd := exec.Command(exe, "--panel-window")
		cmd.Env = append(os.Environ(), "KVMSWITCH_PANEL_URL="+panelSrv.URL(tab))
		err = cmd.Start()
		if err == nil {
			panelPID.Store(uint32(cmd.Process.Pid))
			defer panelPID.Store(0)
			err = cmd.Wait()
		}
	}
	if err != nil {
		log.Printf("面板启动失败: %v", err)
		theCore.P.Notify("面板启动失败", "请检查 WebView2 Runtime 是否已安装，再从托盘重开面板。")
	}
}

// showMenu 是托盘右键菜单：只保留最常用的几项，其余都在操作面板里。
func showMenu() {
	uiMu.Lock()
	st := uiCur
	uiMu.Unlock()
	menu, _, _ := pCreatePopupMenu.Call()
	add := func(flags uintptr, id uintptr, text string) {
		pAppendMenuW.Call(menu, flags, id, uintptr(unsafe.Pointer(u16(text))))
	}
	status := "另一台设备未连接"
	if st.Connected {
		status = "已连接 " + peerLabel(st)
	}
	add(mfString|mfGrayed, 0, status)
	add(mfSeparator, 0, "")
	add(mfString, idPanel, "打开操作面板")
	label := "切换到 " + peerLabel(st)
	if st.Active == core.ActivePeer {
		label = "切回本机"
	}
	add(mfString, idToggle, label+"\t"+theCore.Config().Hotkey)
	add(mfSeparator, 0, "")
	auto := uintptr(mfString)
	if autostartEnabled() {
		auto |= mfChecked
	}
	add(auto, idAutostart, "开机自动启动")
	add(mfSeparator, 0, "")
	add(mfString, idQuit, "退出")

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(hwndMain)
	// TPM_RIGHTBUTTON | TPM_RETURNCMD
	cmd, _, _ := pTrackPopupMenu.Call(menu, 0x2|0x100, uintptr(pt.X), uintptr(pt.Y), 0, hwndMain, 0)
	pDestroyMenu.Call(menu)

	switch cmd {
	case idPanel:
		go openPanel("")
	case idToggle:
		theCore.Toggle()
	case idAutostart:
		if err := setAutostart(!autostartEnabled()); err != nil {
			go messageBox("设置开机自启失败：\n"+err.Error(), "KVMSwitch", mbOK|mbIconWarn)
		}
	case idQuit:
		pPostMessageW.Call(hwndMain, wmClose, 0, 0)
	}
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch uint32(msg) {
	case wmTray:
		switch lParam & 0xFFFF {
		case wmLButtonUp:
			go openPanel("")
		case wmRButtonUp:
			showMenu()
		}
		return 0
	case wmOpenPanel: // 第二次启动程序时由新进程发来
		go openPanel("")
		return 0
	case wmRefresh:
		pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(trayData())))
		return 0
	case wmAwake:
		// 键鼠在另一台设备期间本机收不到任何输入，必须阻止它因“空闲”而睡眠或锁屏。
		// SetThreadExecutionState 按线程生效，所以统一在 UI 线程上调用。
		const esContinuous, esSystem, esDisplay = 0x80000000, 0x1, 0x2
		if keepAwake.Load() {
			pSetThreadExecutionState.Call(esContinuous | esSystem | esDisplay)
		} else {
			pSetThreadExecutionState.Call(esContinuous)
		}
		return 0
	case wmBalloon:
		uiMu.Lock()
		list := balloons
		balloons = nil
		uiMu.Unlock()
		for _, b := range list {
			nid := trayData()
			nid.UFlags |= nifInfo
			copyU16(nid.SzInfoTitle[:], b.title)
			copyU16(nid.SzInfo[:], b.text)
			nid.DwInfoFlags = 1 // NIIF_INFO
			pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(nid)))
		}
		return 0
	case wmDestroy:
		pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(trayData())))
		pPostQuitMessage.Call(0)
		return 0
	}
	if msgTaskbarCreate != 0 && uint32(msg) == msgTaskbarCreate {
		pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(trayData()))) // 资源管理器重启后重新添加图标
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// runUI 创建托盘、安装钩子并运行消息循环；必须在锁定的主线程上调用。
// ready 在窗口和钩子就绪后被调用。
func runUI(ready func()) error {
	hInst, _, _ := pGetModuleHandleW.Call(0)
	cls := u16(wndClassName + instance)
	wc := wndClassEx{LpfnWndProc: windows.NewCallback(wndProc), HInstance: hInst, LpszClassName: cls}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	if r, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassEx: %v", err)
	}
	h, _, err := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(u16("KVMSwitch"))), 0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if h == 0 {
		return fmt.Errorf("CreateWindowEx: %v", err)
	}
	iconSelf = makeIcon(0x2E, 0xA0, 0x43)
	iconPeer = makeIcon(0x2F, 0x6F, 0xEB)
	iconOff = makeIcon(0x80, 0x80, 0x80)
	r, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	msgTaskbarCreate = uint32(r)
	hwndMain = h
	pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(trayData())))

	if err := installHooks(); err != nil {
		return fmt.Errorf("安装键鼠钩子失败: %v", err)
	}
	defer removeHooks()
	ready()

	var m winMsg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return nil
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

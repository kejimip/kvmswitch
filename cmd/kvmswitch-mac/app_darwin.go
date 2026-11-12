package main

import (
	"log"
	"os/exec"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"kvmswitch/internal/core"
)

// Mac 应用外壳：菜单栏图标、内嵌操作面板的原生窗口，以及主线程的应用运行循环。
// 所有界面对象只能在主线程上操作，其它 goroutine 通过 onMain 把工作投递过来。

var (
	nsApp      objc.ID
	appTarget  objc.ID // 接收菜单动作和主线程投递的对象
	statusItem objc.ID
	miStatus   objc.ID
	miToggle   objc.ID
	panelWin   objc.ID
	panelWeb   objc.ID

	mainMu    sync.Mutex
	mainQueue []func()

	// 由 main 注入
	useBrowser bool // 备用方式：不用内嵌窗口，改用默认浏览器显示面板
	uiPanelURL func(tab string) string
	uiToggle   func()
	uiQuit     func()
)

func loadWebKit() error {
	_, err := purego.Dlopen("/System/Library/Frameworks/WebKit.framework/WebKit", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	return err
}

// onMain 把 f 投递到主线程执行。
func onMain(f func()) {
	mainMu.Lock()
	mainQueue = append(mainQueue, f)
	mainMu.Unlock()
	if appTarget != 0 {
		appTarget.Send(sel("performSelectorOnMainThread:withObject:waitUntilDone:"), sel("kvmDrain:"), objc.ID(0), false)
	}
}

func drainMain() {
	mainMu.Lock()
	q := mainQueue
	mainQueue = nil
	mainMu.Unlock()
	for _, f := range q {
		f()
	}
}

func menuItem(menu objc.ID, title, action, key string) objc.ID {
	var s objc.SEL
	if action != "" {
		s = sel(action)
	}
	return menu.Send(sel("addItemWithTitle:action:keyEquivalent:"), nsString(title), s, nsString(key))
}

// buildMainMenu 提供标准的“编辑”菜单，否则面板里的输入框无法使用 Command+C/V。
func buildMainMenu() {
	main := cls("NSMenu").Send(sel("alloc")).Send(sel("initWithTitle:"), nsString("Main"))

	appItem := menuItem(main, "KVMSwitch", "", "")
	appMenu := cls("NSMenu").Send(sel("alloc")).Send(sel("initWithTitle:"), nsString("KVMSwitch"))
	menuItem(appMenu, "关闭窗口", "performClose:", "w")
	menuItem(appMenu, "退出 KVMSwitch", "kvmQuit:", "q").Send(sel("setTarget:"), appTarget)
	appItem.Send(sel("setSubmenu:"), appMenu)

	editItem := menuItem(main, "编辑", "", "")
	edit := cls("NSMenu").Send(sel("alloc")).Send(sel("initWithTitle:"), nsString("编辑"))
	menuItem(edit, "撤销", "undo:", "z")
	menuItem(edit, "剪切", "cut:", "x")
	menuItem(edit, "拷贝", "copy:", "c")
	menuItem(edit, "粘贴", "paste:", "v")
	menuItem(edit, "全选", "selectAll:", "a")
	editItem.Send(sel("setSubmenu:"), edit)

	nsApp.Send(sel("setMainMenu:"), main)
}

func buildStatusItem() {
	bar := cls("NSStatusBar").Send(sel("systemStatusBar"))
	statusItem = bar.Send(sel("statusItemWithLength:"), float64(-1)) // NSVariableStatusItemLength
	statusItem.Send(sel("retain"))
	statusItem.Send(sel("button")).Send(sel("setTitle:"), nsString("KVM ○"))

	menu := cls("NSMenu").Send(sel("alloc")).Send(sel("initWithTitle:"), nsString("KVMSwitch"))
	menu.Send(sel("setAutoenablesItems:"), false)
	miStatus = menuItem(menu, "另一台设备未连接", "", "")
	miStatus.Send(sel("setEnabled:"), false)
	menu.Send(sel("addItem:"), cls("NSMenuItem").Send(sel("separatorItem")))
	menuItem(menu, "打开操作面板", "kvmOpenPanel:", "").Send(sel("setTarget:"), appTarget)
	miToggle = menuItem(menu, "切换", "kvmToggle:", "")
	miToggle.Send(sel("setTarget:"), appTarget)
	menu.Send(sel("addItem:"), cls("NSMenuItem").Send(sel("separatorItem")))
	menuItem(menu, "退出", "kvmQuit:", "").Send(sel("setTarget:"), appTarget)
	statusItem.Send(sel("setMenu:"), menu)
}

// showState 更新菜单栏图标与菜单文字；可在任意 goroutine 调用。
func showState(s core.State) {
	onMain(func() {
		if statusItem == 0 {
			return
		}
		peer := s.PeerName
		if peer == "" {
			peer = "另一台设备"
		}
		title, status, toggle := "KVM ○", "另一台设备未连接", "切换到 "+peer
		if s.Connected {
			status = "已连接 " + peer
			if s.Active == core.ActivePeer {
				title, toggle = "KVM ◐", "切回本机"
			} else {
				title = "KVM ●"
			}
		}
		withPoolMain(func() {
			statusItem.Send(sel("button")).Send(sel("setTitle:"), nsString(title))
			miStatus.Send(sel("setTitle:"), nsString(status))
			miToggle.Send(sel("setTitle:"), nsString(toggle))
			miToggle.Send(sel("setEnabled:"), s.Connected)
		})
	})
}

// withPoolMain 在主线程上包一层自动释放池（主线程已锁定，不需要再锁）。
func withPoolMain(f func()) {
	pool := cls("NSAutoreleasePool").Send(sel("alloc")).Send(sel("init"))
	defer pool.Send(sel("drain"))
	f()
}

// openPanel 显示操作面板窗口；可在任意 goroutine 调用。
func openPanel(tab string) {
	if useBrowser {
		if uiPanelURL != nil {
			exec.Command("/usr/bin/open", uiPanelURL(tab)).Start()
		}
		return
	}
	onMain(func() {
		withPoolMain(func() {
			if uiPanelURL == nil {
				return
			}
			const w, h = 1060.0, 760.0
			if panelWin == 0 {
				// NSWindowStyleMask：标题栏 | 可关闭 | 可最小化 | 可调整大小 = 15；NSBackingStoreBuffered = 2
				panelWin = cls("NSWindow").Send(sel("alloc")).Send(sel("initWithContentRect:styleMask:backing:defer:"),
					0.0, 0.0, w, h, uint(15), uint(2), false)
				panelWin.Send(sel("setReleasedWhenClosed:"), false) // 关闭只是隐藏，程序继续运行
				panelWin.Send(sel("setTitle:"), nsString("KVMSwitch 操作面板"))
				panelWin.Send(sel("center"))
				conf := cls("WKWebViewConfiguration").Send(sel("alloc")).Send(sel("init"))
				panelWeb = cls("WKWebView").Send(sel("alloc")).Send(sel("initWithFrame:configuration:"), 0.0, 0.0, w, h, conf)
				panelWin.Send(sel("setContentView:"), panelWeb)
			}
			visible := objc.Send[bool](panelWin, sel("isVisible"))
			if !visible || tab != "" {
				url := cls("NSURL").Send(sel("URLWithString:"), nsString(uiPanelURL(tab)))
				panelWeb.Send(sel("loadRequest:"), cls("NSURLRequest").Send(sel("requestWithURL:"), url))
			}
			panelWin.Send(sel("makeKeyAndOrderFront:"), objc.ID(0))
			nsApp.Send(sel("activateIgnoringOtherApps:"), true)
		})
	})
}

// runApp 在主线程上运行应用循环，直到退出。ready 在界面就绪后、进入循环前调用。
func runApp(ready func()) error {
	if err := loadWebKit(); err != nil {
		return err
	}
	pool := cls("NSAutoreleasePool").Send(sel("alloc")).Send(sel("init"))

	c, err := objc.RegisterClass("KVMAppTarget", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
		{Cmd: sel("kvmDrain:"), Fn: func(self objc.ID, cmd objc.SEL, arg objc.ID) { drainMain() }},
		{Cmd: sel("kvmOpenPanel:"), Fn: func(self objc.ID, cmd objc.SEL, sender objc.ID) { openPanel("") }},
		{Cmd: sel("kvmToggle:"), Fn: func(self objc.ID, cmd objc.SEL, sender objc.ID) {
			if uiToggle != nil {
				uiToggle()
			}
		}},
		{Cmd: sel("kvmQuit:"), Fn: func(self objc.ID, cmd objc.SEL, sender objc.ID) {
			if uiQuit != nil {
				uiQuit()
			}
			nsApp.Send(sel("terminate:"), objc.ID(0))
		}},
		// 应用已在运行时再次双击图标：打开面板
		{Cmd: sel("applicationShouldHandleReopen:hasVisibleWindows:"), Fn: func(self objc.ID, cmd objc.SEL, app objc.ID, visible bool) bool {
			openPanel("")
			return false
		}},
	})
	if err != nil {
		return err
	}
	nsApp = cls("NSApplication").Send(sel("sharedApplication"))
	nsApp.Send(sel("setActivationPolicy:"), 1) // 辅助应用：不在程序坞显示图标
	appTarget = objc.ID(c).Send(sel("alloc")).Send(sel("init"))
	nsApp.Send(sel("setDelegate:"), appTarget)

	buildMainMenu()
	buildStatusItem()
	pool.Send(sel("drain"))

	ready()
	drainMain()
	log.Printf("界面已就绪")
	nsApp.Send(sel("run"))
	return nil
}

// quitApp 结束应用循环；可在任意 goroutine 调用。
func quitApp() {
	onMain(func() { nsApp.Send(sel("terminate:"), objc.ID(0)) })
}

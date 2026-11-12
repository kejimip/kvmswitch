// KVMSwitch Windows 端。两端运行同一套核心（internal/core），这里只提供 Windows 的系统能力。
package main

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"kvmswitch/internal/core"
	"kvmswitch/internal/panel"
	"kvmswitch/internal/xfer"
)

func init() { runtime.LockOSThread() }

func configDir() string {
	d, _ := os.UserConfigDir() // %APPDATA%
	return filepath.Join(d, "KVMSwitch")
}

func dataDir() string {
	d, _ := os.UserCacheDir() // %LOCALAPPDATA%
	return filepath.Join(d, "KVMSwitch")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }
func logPath() string    { return filepath.Join(dataDir(), "kvmswitch.log") }

// winPlatform 实现 core.Platform 和 panel.Host。
type winPlatform struct {
	inj *injector
}

func (p *winPlatform) OS() string { return "windows" }
func (p *winPlatform) Hostname() string {
	h, _ := os.Hostname()
	return h
}
func (p *winPlatform) RecvDir() string { return filepath.Join(dataDir(), "recv") }

func (p *winPlatform) SetForward(on bool)           { setForward(on) }
func (p *winPlatform) CanControl() bool             { return true }
func (p *winPlatform) SetControlled(on bool)        { p.inj.setActive(on) }
func (p *winPlatform) ApplyTuning(cfg *core.Config) { p.inj.tune(cfg) }
func (p *winPlatform) InjectKey(vk uint16, flags byte) bool {
	p.inj.key(vk, flags)
	return false // Mac 的访达没有“剪切文件”，所以 Windows 端不需要识别粘贴
}
func (p *winPlatform) InjectMove(dx, dy int32)                        { p.inj.move(dx, dy) }
func (p *winPlatform) InjectButton(b int, down bool)                  { p.inj.button(b, down) }
func (p *winPlatform) InjectWheel(dy, dx int32)                       { p.inj.wheel(dy, dx) }
func (p *winPlatform) SetMonitorInput(v uint16, _ *core.Config) error { return ddcSetInput(v) }

func (p *winPlatform) ClipChange() uint64           { return uint64(clipboardSeq()) }
func (p *winPlatform) ReadClip() (xfer.Clip, bool)  { return readClipboard() }
func (p *winPlatform) Recycle(paths []string) error { return recycle(paths) }
func (p *winPlatform) WriteClip(kind string, data []byte, paths []string) (uint64, error) {
	seq, err := writeClipboard(kind, data, paths)
	return uint64(seq), err
}

func (p *winPlatform) Confirm(msg string) bool {
	return messageBoxTimeout(msg+"（60 秒内不选择则取消）", "KVMSwitch", mbYesNo|mbIconQuest, 60000) == idYes
}
func (p *winPlatform) Notify(title, text string) { notify(title, text) }
func (p *winPlatform) Beep()                     { pMessageBeep.Call(0x30) }
func (p *winPlatform) OnState(s core.State)      { uiRefresh(s) }
func (p *winPlatform) KeepAwake(on bool) {
	keepAwake.Store(on)
	if hwndMain != 0 {
		pPostMessageW.Call(hwndMain, wmAwake, 0, 0)
	}
}

// panel.Host
func (p *winPlatform) Autostart() bool            { return autostartEnabled() }
func (p *winPlatform) SetAutostart(on bool) error { return setAutostart(on) }
func (p *winPlatform) MonitorInfo() string        { return ddcReport() }
func (p *winPlatform) Quit() {
	time.Sleep(200 * time.Millisecond)
	pPostMessageW.Call(hwndMain, wmClose, 0, 0)
}
func (p *winPlatform) CopyText(s string) error {
	_, err := writeClipboard(xfer.KindText, []byte(s), nil)
	return err
}
func (p *winPlatform) Open(what string) error {
	switch what {
	case "log":
		shellOpen(logPath())
	case "config":
		shellOpen(configPath())
	case "recv":
		os.MkdirAll(p.RecvDir(), 0o755)
		shellOpen(p.RecvDir())
	default:
		return errors.New("这一项在 Windows 上不适用")
	}
	return nil
}

func fatal(msg string) {
	log.Print(msg)
	messageBox(msg, "KVMSwitch", mbOK|mbIconWarn)
	os.Exit(1)
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--panel-window" {
		os.Exit(runPanelWindow())
	}
	// --background：开机自启时使用，不弹出面板
	// --restart：等待旧进程退出后接管
	var background, restart bool
	for _, a := range os.Args[1:] {
		switch a {
		case "--background":
			background = true
		case "--restart":
			restart = true
		}
	}

	// 让钩子坐标与 GetCursorPos 使用同一套物理像素
	if pSetProcessDpiCtx.Find() == nil {
		pSetProcessDpiCtx.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2 = -4
	}

	// 单实例：再次启动时只是让已运行的实例打开面板
	// KVM_INSTANCE 仅用于调试：让一个测试实例与正式实例并存
	instance = os.Getenv("KVM_INSTANCE")
	name, _ := windows.UTF16PtrFromString(`Local\KVMSwitchSingleton` + instance)
	deadline := time.Now().Add(5 * time.Second)
	for {
		h, err := windows.CreateMutex(nil, false, name)
		if err == nil {
			break
		}
		if h != 0 {
			windows.CloseHandle(h) // 不关闭的话，旧进程退出后互斥体仍被我们自己占着
		}
		if err == windows.ERROR_ALREADY_EXISTS && restart && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if w, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(u16(wndClassName+instance))), 0); w != 0 {
			if !background {
				pPostMessageW.Call(w, wmOpenPanel, 0, 0)
			}
			return
		}
		messageBox("KVMSwitch 已经在运行（见任务栏右下角托盘图标）。", "KVMSwitch", mbOK|mbIconInfo)
		return
	}

	os.MkdirAll(dataDir(), 0o755)
	if st, err := os.Stat(logPath()); err == nil && st.Size() > 2<<20 {
		os.Remove(logPath())
	}
	logs := &panel.LogRing{}
	if f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(f, logs))
	} else {
		log.SetOutput(logs)
	}

	p := &winPlatform{inj: newInjector()}
	c, created, err := core.New(p, core.Options{ConfigPath: configPath(), Listen: true})
	if err != nil {
		fatal("读取配置失败：\n" + err.Error() + "\n\n配置文件：" + configPath())
	}
	p.inj.fetch = c.RequestPaste
	theCore = c
	xfer.CleanOld(p.RecvDir(), 3)
	if autostartEnabled() {
		setAutostart(true) // 刷新注册表里的路径和启动参数
	}

	err = runUI(func() {
		if err := c.Start(); err != nil {
			fatal("无法监听端口，程序可能已在运行或端口被占用：\n" + err.Error())
		}
		if panelSrv, err = panel.Start(c, p, logs, false); err != nil {
			log.Printf("操作面板不可用: %v", err)
		}
		log.Printf("KVMSwitch %s 已启动，热键 %s，紧急热键 %s", core.Version, c.Config().Hotkey, c.Config().EmergencyHotkey)
		switch {
		case created:
			go openPanel("pair") // 首次运行：直接进入连接引导
		case !background:
			go openPanel("home")
		}
	})
	if err != nil {
		fatal(err.Error())
	}
}

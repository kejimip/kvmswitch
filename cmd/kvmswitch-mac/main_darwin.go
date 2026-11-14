// KVMSwitch Mac 端。两端运行同一套核心（internal/core），这里只提供 macOS 的系统能力。
package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kvmswitch/internal/core"
	"kvmswitch/internal/panel"
	"kvmswitch/internal/xfer"
)

const agentLabel = "com.kvmswitch.app"

func init() { runtime.LockOSThread() } // 界面必须运行在主线程

func supportDir() string {
	d, _ := os.UserConfigDir() // ~/Library/Application Support
	return filepath.Join(d, "KVMSwitch")
}

func cacheDir() string {
	d, _ := os.UserCacheDir() // ~/Library/Caches
	return filepath.Join(d, "KVMSwitch")
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func cfgPath() string  { return filepath.Join(supportDir(), "config.json") }
func sockPath() string { return filepath.Join(supportDir(), "ctl.sock") }
func logPath() string  { return filepath.Join(homeDir(), "Library", "Logs", "KVMSwitch.log") }
func plistPath() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", agentLabel+".plist")
}

func osa(script string) (string, error) {
	out, err := exec.Command("/usr/bin/osascript", "-e", script).Output()
	return string(out), err
}

// macPlatform 实现 core.Platform 和 panel.Host。
type macPlatform struct {
	inj  *injector
	tap  *tap
	core *core.Core

	mu    sync.Mutex
	awake *exec.Cmd
	host  string
}

func (p *macPlatform) OS() string { return "mac" }

func (p *macPlatform) Hostname() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == "" {
		if out, err := exec.Command("/usr/sbin/scutil", "--get", "ComputerName").Output(); err == nil {
			p.host = strings.TrimSpace(string(out))
		}
		if p.host == "" {
			h, _ := os.Hostname()
			p.host = strings.TrimSuffix(h, ".local")
		}
	}
	return p.host
}

func (p *macPlatform) RecvDir() string { return filepath.Join(cacheDir(), "recv") }

func (p *macPlatform) SetForward(on bool) {
	if p.tap != nil {
		p.tap.setForward(on)
	}
}
func (p *macPlatform) CanControl() bool      { return cgCanPostEvents() }
func (p *macPlatform) SetControlled(on bool) { p.inj.setActive(on) }
func (p *macPlatform) ApplyTuning(cfg *core.Config) {
	p.inj.tune(cfg)
	if p.tap != nil {
		p.tap.tune(cfg)
	}
}
func (p *macPlatform) InjectKey(vk uint16, flags byte) bool { return p.inj.key(vk, flags) }
func (p *macPlatform) InjectMove(dx, dy int32)              { p.inj.move(dx, dy) }
func (p *macPlatform) InjectButton(b int, down bool)        { p.inj.button(b, down) }
func (p *macPlatform) InjectWheel(dy, dx int32)             { p.inj.wheel(dy, dx) }

func (p *macPlatform) SetMonitorInput(v uint16, cfg *core.Config) error {
	return ddcSetInput(v, cfg.MacDDCCommand, cfg.MonitorID)
}

func (p *macPlatform) ClipChange() uint64          { return uint64(pbChangeCount()) }
func (p *macPlatform) ReadClip() (xfer.Clip, bool) { return readPasteboard() }
func (p *macPlatform) WriteClip(kind string, data []byte, paths []string, expected uint64) (uint64, error) {
	n, err := writePasteboard(kind, data, paths, &expected)
	return uint64(n), err
}

func (p *macPlatform) Confirm(msg string) bool {
	script := fmt.Sprintf(`display dialog %q buttons {"取消", "接收"} default button "接收" with title "KVMSwitch" giving up after 60`, msg)
	out, err := osa(script)
	return err == nil && strings.Contains(out, "button returned:接收")
}

func (p *macPlatform) Notify(title, text string) {
	go osa(fmt.Sprintf(`display notification %q with title %q`, text, "KVMSwitch："+title))
}

func (p *macPlatform) Beep()                { go osa("beep") }
func (p *macPlatform) OnState(s core.State) { showState(s) }

// KeepAwake：键鼠在另一台设备、或本机靠注入的输入被控制时，系统都可能判定“空闲”而休眠，需要阻止。
func (p *macPlatform) KeepAwake(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on && p.awake == nil {
		cmd := exec.Command("/usr/bin/caffeinate", "-d", "-i", "-w", strconv.Itoa(os.Getpid()))
		if cmd.Start() == nil {
			p.awake = cmd
		}
	} else if !on && p.awake != nil {
		p.awake.Process.Kill()
		go p.awake.Wait()
		p.awake = nil
	}
}

// ---- panel.Host

func (p *macPlatform) Autostart() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// SetAutostart 写入或删除登录项（LaunchAgent）。下次登录时生效，不影响当前正在运行的程序。
func (p *macPlatform) SetAutostart(on bool) error {
	if !on {
		err := os.Remove(plistPath())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string><string>--background</string></array>
	<key>RunAtLoad</key><true/>
	<key>LimitLoadToSessionType</key><string>Aqua</string>
	<key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`, agentLabel, xmlEscape(exe))
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(plistPath(), []byte(plist), 0o644)
}

func (p *macPlatform) Open(what string) error {
	var target string
	switch what {
	case "log":
		target = logPath()
	case "config":
		target = cfgPath()
	case "recv":
		os.MkdirAll(p.RecvDir(), 0o755)
		target = p.RecvDir()
	case "accessibility":
		if !p.CanControl() {
			cgRequestPostEvents()
		}
		target = "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
	default:
		return errors.New("未知的目标")
	}
	args := []string{target}
	if what == "log" || what == "config" {
		args = []string{"-t", target} // 用文本编辑器打开
	}
	return exec.Command("/usr/bin/open", args...).Start()
}

func (p *macPlatform) Quit() {
	time.Sleep(200 * time.Millisecond)
	p.cleanup()
	quitApp()
}

func (p *macPlatform) cleanup() {
	p.SetForward(false) // 确保退出时光标不会停留在“冻结”状态
	p.KeepAwake(false)
	os.Remove(sockPath())
}

func (p *macPlatform) MonitorInfo() string {
	return ddcProbe(p.core.Config().MacDDCCommand)
}

func (p *macPlatform) CopyText(s string) error {
	_, err := writePasteboard(xfer.KindText, []byte(s), nil, nil)
	return err
}

// ---------------------------------------------------------------- 入口

func fatal(msg string) {
	log.Print(msg)
	osa(fmt.Sprintf(`display alert "KVMSwitch" message %q as critical`, msg))
	os.Exit(1)
}

// alreadyRunning 检查是否已有实例在运行；有的话请它打开面板。
func alreadyRunning(openPanel bool) bool {
	conn, err := net.DialTimeout("unix", sockPath(), time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	if openPanel {
		conn.Write([]byte("panel\n"))
	}
	return true
}

// serveControl 让之后启动的实例可以请求本实例打开面板。
func serveControl() {
	os.Remove(sockPath())
	ln, err := net.Listen("unix", sockPath())
	if err != nil {
		log.Printf("单实例检测不可用: %v", err)
		return
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 16)
			conn.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := conn.Read(buf)
			conn.Close()
			if strings.HasPrefix(string(buf[:n]), "panel") {
				openPanel("")
			}
		}
	}()
}

func main() {
	var background bool
	args := os.Args[1:]
	for _, a := range args {
		switch a {
		case "--background":
			background = true
		case "--browser":
			useBrowser = true
		}
	}
	for _, load := range []func() error{loadCG, loadAppKit, loadTap} {
		if err := load(); err != nil {
			fmt.Println("加载系统框架失败:", err)
			os.Exit(1)
		}
	}

	// 诊断命令：./kvmswitch-mac ddc <输入口>
	if len(args) >= 1 && args[0] == "ddc" {
		if len(args) < 2 {
			fmt.Println("用法：kvmswitch-mac ddc <hdmi1|hdmi2|dp1|dp2|数字>")
			os.Exit(1)
		}
		v, err := core.ParseInput(args[1])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if err := ddcSetInput(v, "", ""); err != nil {
			fmt.Println("失败:", err)
			os.Exit(1)
		}
		fmt.Printf("已发送切换指令（输入源 0x%02X）。\n", v)
		return
	}

	os.MkdirAll(supportDir(), 0o700)
	if alreadyRunning(!background) {
		return
	}

	os.MkdirAll(filepath.Dir(logPath()), 0o755)
	if st, err := os.Stat(logPath()); err == nil && st.Size() > 2<<20 {
		os.Remove(logPath())
	}
	logs := &panel.LogRing{}
	if f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(f, logs))
	} else {
		log.SetOutput(io.MultiWriter(os.Stderr, logs))
	}

	p := &macPlatform{inj: newInjector()}
	c, created, err := core.New(p, core.Options{ConfigPath: cfgPath(), Listen: false})
	if err != nil {
		fatal("读取配置失败：" + err.Error() + "\n\n配置文件：" + cfgPath())
	}
	defer c.Files.Forget()
	p.inj.fetch = c.RequestPaste
	p.core = c
	p.tap = newTap(c, p.inj)
	p.tap.tune(c.Config())
	xfer.CleanOld(p.RecvDir(), 3)
	if p.Autostart() {
		p.SetAutostart(true) // 刷新登录项里的程序路径
	}
	serveControl()

	uiToggle = c.Toggle
	uiQuit = p.cleanup
	err = runApp(func() {
		p.tap.start()
		if err := c.Start(); err != nil {
			fatal(err.Error())
		}
		srv, err := panel.Start(c, p, logs, true)
		if err != nil {
			log.Printf("操作面板不可用: %v", err)
		} else {
			uiPanelURL = srv.URL
		}
		log.Printf("KVMSwitch %s 已启动，热键 %s，紧急热键 %s", core.Version, c.Config().Hotkey, c.Config().EmergencyHotkey)
		canPost := p.CanControl()
		exe, _ := os.Executable()
		log.Printf("键鼠权限检查：PostEvent=%t，AX=%t，程序=%s", canPost, axIsProcessTrusted(), exe)
		if !canPost {
			cgRequestPostEvents()
		}
		switch {
		case created:
			openPanel("pair") // 首次运行：直接进入连接引导
		case !background:
			openPanel("home")
		}
	})
	if err != nil {
		fatal("界面启动失败：" + err.Error())
	}
}

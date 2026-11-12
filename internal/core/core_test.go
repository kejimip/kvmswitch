package core

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kvmswitch/internal/proto"
	"kvmswitch/internal/xfer"
)

// fake 是一个模拟的操作系统：记录核心对它的每一次调用。
type fake struct {
	mu         sync.Mutex
	os, name   string
	dir        string
	forward    bool
	controlled bool
	awake      bool
	canControl bool
	keys       []uint16
	moves      [][2]int32
	buttons    int
	wheels     int
	monitor    []uint16
	monitorErr error
	clipChg    uint64
	clip       *xfer.Clip
	written    []xfer.Clip
	recycled   []string
	notes      []string
	beeps      int
	pasteOn    uint16 // 注入这个键时报告“发生了粘贴”
	tuned      *Config
}

func (f *fake) lock() func()     { f.mu.Lock(); return f.mu.Unlock }
func (f *fake) OS() string       { return f.os }
func (f *fake) Hostname() string { return f.name }
func (f *fake) RecvDir() string  { return f.dir }
func (f *fake) SetForward(on bool) {
	defer f.lock()()
	f.forward = on
}
func (f *fake) CanControl() bool {
	defer f.lock()()
	return f.canControl
}
func (f *fake) SetControlled(on bool) {
	defer f.lock()()
	f.controlled = on
}
func (f *fake) ApplyTuning(c *Config) {
	defer f.lock()()
	f.tuned = c
}
func (f *fake) InjectKey(vk uint16, flags byte) bool {
	defer f.lock()()
	f.keys = append(f.keys, vk)
	return f.pasteOn != 0 && vk == f.pasteOn && flags&proto.KeyFlagDown != 0
}
func (f *fake) InjectMove(dx, dy int32) {
	defer f.lock()()
	f.moves = append(f.moves, [2]int32{dx, dy})
}
func (f *fake) InjectButton(int, bool) {
	defer f.lock()()
	f.buttons++
}
func (f *fake) InjectWheel(int32, int32) {
	defer f.lock()()
	f.wheels++
}
func (f *fake) SetMonitorInput(v uint16, _ *Config) error {
	defer f.lock()()
	f.monitor = append(f.monitor, v)
	return f.monitorErr
}
func (f *fake) ClipChange() uint64 {
	defer f.lock()()
	return f.clipChg
}
func (f *fake) ReadClip() (xfer.Clip, bool) {
	defer f.lock()()
	if f.clip == nil {
		return xfer.Clip{}, false
	}
	return *f.clip, true
}
func (f *fake) WriteClip(kind string, data []byte, paths []string) (uint64, error) {
	defer f.lock()()
	f.clipChg++
	f.written = append(f.written, xfer.Clip{Kind: kind, Data: data, Paths: paths})
	return f.clipChg, nil
}
func (f *fake) Recycle(p []string) error {
	defer f.lock()()
	f.recycled = append(f.recycled, p...)
	return nil
}
func (f *fake) Confirm(string) bool { return true }
func (f *fake) Notify(title, text string) {
	defer f.lock()()
	f.notes = append(f.notes, title)
}
func (f *fake) Beep() {
	defer f.lock()()
	f.beeps++
}
func (f *fake) OnState(State) {}
func (f *fake) KeepAwake(on bool) {
	defer f.lock()()
	f.awake = on
}

// copyText 模拟用户在这台设备上复制了一段文字。
func (f *fake) copyText(s string) {
	defer f.lock()()
	f.clipChg++
	f.clip = &xfer.Clip{Kind: xfer.KindText, Data: []byte(s)}
}

func (f *fake) get(fn func()) {
	defer f.lock()()
	fn()
}

type rig struct {
	t        *testing.T
	win, mac *Core
	fw, fm   *fake
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// newRig 启动一台“Windows”（接听）和一台“Mac”（拨号），两者使用相同的核心代码。
func newRig(t *testing.T, sameKey bool) *rig {
	t.Helper()
	port := freePort(t)
	mk := func(osName, name, key string, listen bool) (*Core, *fake) {
		dir := t.TempDir()
		cfg := DefaultConfig()
		cfg.PairingKey, cfg.Port = key, port
		if !listen {
			cfg.PeerHost = fmt.Sprintf("127.0.0.1:%d", port)
		}
		path := filepath.Join(dir, "config.json")
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		f := &fake{os: osName, name: name, dir: filepath.Join(dir, "recv"), canControl: true}
		c, _, err := New(f, Options{ConfigPath: path, Listen: listen})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		return c, f
	}
	r := &rig{t: t}
	macKey := "AAAAA-AAAAA-AAAAA-AAAAA"
	if !sameKey {
		macKey = "BBBBB-BBBBB-BBBBB-BBBBB"
	}
	r.win, r.fw = mk("windows", "my-pc", "AAAAA-AAAAA-AAAAA-AAAAA", true)
	r.mac, r.fm = mk("mac", "my-mac", macKey, false)
	return r
}

func (r *rig) wait(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	r.t.Fatalf("等待超时：%s（win=%+v mac=%+v）", what, r.win.State(), r.mac.State())
}

func (r *rig) connected() {
	r.t.Helper()
	r.wait("两端互相识别", func() bool {
		a, b := r.win.State(), r.mac.State()
		return a.Connected && b.Connected && a.PeerName == "my-mac" && b.PeerName == "my-pc" && a.PeerCanControl && b.PeerCanControl
	})
}

func (r *rig) active(win, mac string) {
	r.t.Helper()
	r.wait("键鼠归属 win="+win+" mac="+mac, func() bool {
		return r.win.State().Active == win && r.mac.State().Active == mac
	})
}

func TestConnectAndInfo(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	if s := r.win.State(); s.PeerOS != "mac" || s.Active != ActiveNone {
		t.Fatalf("%+v", s)
	}
}

// 键鼠插在 Windows 上：按热键切到 Mac，再按一次切回。
func TestToggleFromWindowsKeyboard(t *testing.T) {
	r := newRig(t, true)
	r.connected()

	r.win.Hotkey(false)
	r.active(ActivePeer, ActiveSelf)
	r.wait("Windows 开始转发、Mac 开始被控", func() bool {
		var ok bool
		r.fw.get(func() { ok = r.fw.forward && !r.fw.controlled && r.fw.awake })
		r.fm.get(func() { ok = ok && !r.fm.forward && r.fm.controlled && r.fm.awake })
		return ok
	})
	// 两端都应尝试把显示器切到 Mac 的输入口 (dp1 = 0x0F)
	r.wait("两端都发出显示器指令", func() bool {
		var a, b bool
		r.fw.get(func() { a = len(r.fw.monitor) == 1 && r.fw.monitor[0] == 0x0F })
		r.fm.get(func() { b = len(r.fm.monitor) == 1 && r.fm.monitor[0] == 0x0F })
		return a && b
	})

	// Windows 上采集到的输入应到达 Mac
	r.win.PushInput(InputEvent{Typ: proto.MsgKey, A: 0x41, C: proto.KeyFlagDown})
	r.win.PushInput(InputEvent{Typ: proto.MsgMouseMove, A: 3, B: -2})
	r.win.PushInput(InputEvent{Typ: proto.MsgMouseMove, A: 4, B: 1})
	r.win.PushInput(InputEvent{Typ: proto.MsgMouseButton, A: proto.BtnLeft, C: 1})
	r.win.PushInput(InputEvent{Typ: proto.MsgWheel, A: 120})
	r.wait("输入到达 Mac", func() bool {
		var ok bool
		r.fm.get(func() {
			var sx, sy int32
			for _, m := range r.fm.moves {
				sx, sy = sx+m[0], sy+m[1]
			}
			ok = len(r.fm.keys) == 1 && r.fm.keys[0] == 0x41 && sx == 7 && sy == -1 && r.fm.buttons == 1 && r.fm.wheels == 1
		})
		return ok
	})

	r.win.Hotkey(false)
	r.active(ActiveSelf, ActivePeer)
	r.wait("Windows 停止转发", func() bool {
		var ok bool
		r.fw.get(func() { ok = !r.fw.forward && r.fw.controlled })
		r.fm.get(func() { ok = ok && r.fm.forward && !r.fm.controlled })
		return ok
	})
	r.wait("显示器切回 Windows 输入口", func() bool {
		var ok bool
		r.fm.get(func() { ok = len(r.fm.monitor) == 2 && r.fm.monitor[1] == 0x11 })
		return ok
	})
}

// 没有主从：键鼠插在 Mac 上时，同样的操作反过来也成立。
func TestToggleFromMacKeyboard(t *testing.T) {
	r := newRig(t, true)
	r.connected()

	r.mac.Hotkey(false)
	r.active(ActiveSelf, ActivePeer)
	r.mac.PushInput(InputEvent{Typ: proto.MsgKey, A: 0x42, C: proto.KeyFlagDown})
	r.wait("Mac 的输入到达 Windows", func() bool {
		var ok bool
		r.fw.get(func() { ok = len(r.fw.keys) == 1 && r.fw.keys[0] == 0x42 })
		return ok
	})
	r.wait("显示器切到 Windows 输入口", func() bool {
		var ok bool
		r.fm.get(func() { ok = len(r.fm.monitor) == 1 && r.fm.monitor[0] == 0x11 })
		return ok
	})
	r.mac.Hotkey(false)
	r.active(ActivePeer, ActiveSelf)
}

// 键鼠不在本机时，本机不应把收到的输入注入。
func TestNoInjectionWhenNotActive(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	r.win.PushInput(InputEvent{Typ: proto.MsgKey, A: 0x41, C: proto.KeyFlagDown}) // 尚未切换
	time.Sleep(300 * time.Millisecond)
	r.fm.get(func() {
		if len(r.fm.keys) != 0 {
			t.Fatal("未切换时不应注入")
		}
	})
}

// 紧急热键：在哪台设备上按，键鼠就回到哪台。
func TestEmergencyTakesControlHere(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	r.win.Hotkey(false)
	r.active(ActivePeer, ActiveSelf)
	r.win.Hotkey(true)
	r.active(ActiveSelf, ActivePeer)
	r.mac.Hotkey(true)
	r.active(ActivePeer, ActiveSelf)
}

func TestClipboardFollowsSwitch(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	r.fw.copyText("来自 Windows")
	r.win.Hotkey(false)
	r.active(ActivePeer, ActiveSelf)
	r.wait("Mac 收到剪贴板", func() bool {
		var ok bool
		r.fm.get(func() { ok = len(r.fm.written) == 1 && string(r.fm.written[0].Data) == "来自 Windows" })
		return ok
	})
	// 切回：Mac 上没有新复制的内容，不应把刚收到的内容原样弹回去
	r.win.Hotkey(false)
	r.active(ActiveSelf, ActivePeer)
	time.Sleep(300 * time.Millisecond)
	r.fw.get(func() {
		if len(r.fw.written) != 0 {
			t.Fatal("刚收到的内容不应被回传")
		}
	})
	// 再切过去并在 Mac 上复制，切回后 Windows 应收到
	r.win.Hotkey(false)
	r.active(ActivePeer, ActiveSelf)
	r.fm.copyText("来自 Mac")
	r.win.Hotkey(false)
	r.active(ActiveSelf, ActivePeer)
	r.wait("Windows 收到剪贴板", func() bool {
		var ok bool
		r.fw.get(func() { ok = len(r.fw.written) == 1 && string(r.fw.written[0].Data) == "来自 Mac" })
		return ok
	})
}

// 在 Windows 剪切文件 -> 切到 Mac -> 按粘贴键 -> Windows 源文件进回收站。
func TestCutThenPasteRecycles(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	src := filepath.Join(t.TempDir(), "报告.txt")
	if err := os.WriteFile(src, []byte("内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.fw.get(func() {
		r.fw.clipChg++
		r.fw.clip = &xfer.Clip{Kind: xfer.KindFiles, Paths: []string{src}, Cut: true}
	})
	r.fm.get(func() { r.fm.pasteOn = 0x56 })
	r.win.Hotkey(false)
	r.active(ActivePeer, ActiveSelf)
	r.wait("文件到达 Mac", func() bool {
		var ok bool
		r.fm.get(func() { ok = len(r.fm.written) == 1 && len(r.fm.written[0].Paths) == 1 })
		return ok
	})
	r.fw.get(func() {
		if len(r.fw.recycled) != 0 {
			t.Fatal("还没粘贴就移走了源文件")
		}
	})
	r.win.PushInput(InputEvent{Typ: proto.MsgKey, A: 0x56, C: proto.KeyFlagDown})
	r.wait("源文件进回收站", func() bool {
		var ok bool
		r.fw.get(func() { ok = len(r.fw.recycled) == 1 && r.fw.recycled[0] == src })
		return ok
	})
	// 第二次粘贴不应重复触发
	r.win.PushInput(InputEvent{Typ: proto.MsgKey, A: 0x56, C: proto.KeyFlagDown})
	time.Sleep(300 * time.Millisecond)
	r.fw.get(func() {
		if len(r.fw.recycled) != 1 {
			t.Fatal("重复移入回收站")
		}
	})
}

// 在任意一端改设置，另一端自动跟上。
func TestConfigSyncBothWays(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	next := *r.win.Config()
	next.Hotkey, next.MouseScale = "Ctrl+F11", 1.7
	if err := r.win.ApplyConfig(&next); err != nil {
		t.Fatal(err)
	}
	r.wait("Mac 同步到 Windows 的改动", func() bool {
		c := r.mac.Config()
		return c.Hotkey == "Ctrl+F11" && c.MouseScale == 1.7
	})
	if r.mac.Config().PairingKey != "AAAAA-AAAAA-AAAAA-AAAAA" || r.mac.Config().PeerHost == "" {
		t.Fatal("同步不应覆盖每台设备各自的配对信息")
	}
	next = *r.mac.Config()
	next.MacInput = "dp2"
	if err := r.mac.ApplyConfig(&next); err != nil {
		t.Fatal(err)
	}
	r.wait("Windows 同步到 Mac 的改动", func() bool {
		c := r.win.Config()
		return c.MacInput == "dp2" && c.Hotkey == "Ctrl+F11"
	})
	// 同步来的设置必须真的生效，而不只是存下来
	r.fm.get(func() {
		if r.fm.tuned == nil || r.fm.tuned.MouseScale != 1.7 {
			t.Fatal("同步来的设置没有应用到平台")
		}
	})
	if err := r.win.ApplyConfig(&Config{Hotkey: "K"}); err == nil {
		t.Fatal("非法设置应被拒绝")
	}
}

// 两端都无法让显示器换口时，撤销这次切换并提示。
func TestMonitorFailureReverts(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	r.fw.get(func() { r.fw.monitorErr = errors.New("不支持") })
	r.fm.get(func() { r.fm.monitorErr = errors.New("不支持") })
	r.win.Hotkey(false)
	r.wait("切换被撤销并提示", func() bool {
		var noted bool
		r.fw.get(func() { noted = len(r.fw.notes) > 0 })
		return noted && r.win.State().Active == ActiveNone && r.mac.State().Active == ActiveNone
	})
	r.fw.get(func() {
		if r.fw.forward {
			t.Fatal("撤销后不应继续转发")
		}
	})
}

// 只要有一端成功让显示器换口，切换就成立。
func TestMonitorOneSideEnough(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	r.fm.get(func() { r.fm.monitorErr = errors.New("当前不是我的输入口") })
	r.win.Hotkey(false)
	r.active(ActivePeer, ActiveSelf)
	time.Sleep(3500 * time.Millisecond) // 超过裁决等待时间
	if r.win.State().Active != ActivePeer {
		t.Fatal("不应撤销")
	}
}

func TestSwitchRefusedWithoutPermission(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	r.fm.get(func() { r.fm.canControl = false })
	r.wait("Windows 得知 Mac 尚未授权", func() bool { return !r.win.State().PeerCanControl })
	r.win.Hotkey(false)
	r.wait("拒绝切换并提示", func() bool {
		var ok bool
		r.fw.get(func() { ok = r.fw.beeps == 1 && len(r.fw.notes) == 1 })
		return ok
	})
	if r.win.State().Active != ActiveNone {
		t.Fatal("不应切换")
	}
}

func TestWrongKeyThenPair(t *testing.T) {
	r := newRig(t, false)
	time.Sleep(1200 * time.Millisecond)
	if r.win.State().Connected || r.mac.State().Connected {
		t.Fatal("密钥不同不应连上")
	}
	if err := r.mac.SetPairing("not-a-key", ""); err == nil {
		t.Fatal("格式错误的密钥应被拒绝")
	}
	// 在 Mac 的面板里填入 Windows 的密钥（小写、无分隔符也应接受）
	if err := r.mac.SetPairing("aaaaaaaaaaaaaaaaaaaa", r.mac.Config().PeerHost); err != nil {
		t.Fatal(err)
	}
	r.connected()
	if r.mac.Config().PairingKey != "AAAAA-AAAAA-AAAAA-AAAAA" {
		t.Fatalf("密钥未规范化: %q", r.mac.Config().PairingKey)
	}
}

func TestSwitchWithoutPeer(t *testing.T) {
	r := newRig(t, false) // 密钥不同，永远连不上
	r.win.Hotkey(false)
	r.wait("提示未连接", func() bool {
		var ok bool
		r.fw.get(func() { ok = r.fw.beeps == 1 })
		return ok
	})
	r.fw.get(func() {
		if r.fw.forward {
			t.Fatal("未连接时绝不能开始吞掉本机输入")
		}
	})
}

func TestHotkeyHelpers(t *testing.T) {
	for _, s := range []string{"Ctrl+F12", "Ctrl+Alt+Shift+F12", "ScrollLock", "Win+K", "Ctrl+Alt+Shift+Win+9", "Alt+Space", "F24", "Ctrl+`"} {
		h, err := ParseHotkey(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := FormatHotkey(h); got != s {
			t.Errorf("FormatHotkey(Parse(%q)) = %q", s, got)
		}
		if Unpack(h.Pack()) != h {
			t.Errorf("%q Pack 后无法还原", s)
		}
	}
	for _, bad := range []string{"", "Ctrl+", "Ctrl", "Ctrl+F12+F11", "Ctrl+F99", "Ctrl+??"} {
		if _, err := ParseHotkey(bad); err == nil {
			t.Errorf("%q 应当报错", bad)
		}
	}
	if FormatHotkey(Hotkey{VK: 0xE7}) != "" {
		t.Error("不支持的键应返回空串")
	}
}

func TestKeysDecision(t *testing.T) {
	var k Keys
	tg, _ := ParseHotkey("Ctrl+F12")
	em, _ := ParseHotkey("Ctrl+Alt+Shift+F12")
	k.Set(tg, em)
	if k.OnKeyDown(0x7B, ModCtrl, false) != KeyToggle || k.OnKeyDown(0x7B, ModCtrl|ModAlt|ModShift, false) != KeyEmergency {
		t.Fatal("热键判定不对")
	}
	// 修饰键必须完全一致才算命中
	if k.OnKeyDown(0x7B, ModCtrl|ModShift, false) != KeyPass || k.OnKeyDown(0x7A, ModCtrl, false) != KeyPass {
		t.Fatal("不应命中")
	}
	// 录制期间：热键不触发，按键交给面板；修饰键本身放行
	k.BeginCapture()
	if k.OnKeyDown(0xA2, ModCtrl, true) != KeyPass {
		t.Fatal("录制时修饰键应放行")
	}
	if k.OnKeyDown(0x7B, ModCtrl, false) != KeySwallow {
		t.Fatal("录制时应吞掉并记录")
	}
	if h, ok := k.PollCapture(); !ok || FormatHotkey(h) != "Ctrl+F12" {
		t.Fatalf("录制结果不对: %+v %v", h, ok)
	}
	if _, ok := k.PollCapture(); ok {
		t.Fatal("结果只能取走一次")
	}
	k.EndCapture()
	if k.OnKeyDown(0x7B, ModCtrl, false) != KeyToggle {
		t.Fatal("结束录制后热键应恢复")
	}
}

func TestValidateAndKeys(t *testing.T) {
	ok := DefaultConfig()
	p, err := Validate(ok)
	if err != nil || p.WinInput != 0x11 || p.MacInput != 0x0F || p.Toggle.VK != 0x7B {
		t.Fatalf("默认配置应当合法: %+v %v", p, err)
	}
	bad := map[string]func(c *Config){
		"单独字母做热键":  func(c *Config) { c.Hotkey = "K" },
		"Shift+字母": func(c *Config) { c.Hotkey = "Shift+K" },
		"两个热键相同":   func(c *Config) { c.EmergencyHotkey = c.Hotkey },
		"输入口相同":    func(c *Config) { c.MacInput = "HDMI1" },
		"输入口无法识别":  func(c *Config) { c.MacInput = "vga" },
		"非法修饰键":    func(c *Config) { c.ModWin = "hyper" },
		"修饰键重复":    func(c *Config) { c.ModWin = "control" },
		"鼠标倍率为 0":  func(c *Config) { c.MouseScale = 0 },
		"上限为负":     func(c *Config) { c.MaxAutoFileMB = -1 },
	}
	for name, mut := range bad {
		c := *ok
		mut(&c)
		if _, err := Validate(&c); err == nil {
			t.Errorf("%s：应当报错", name)
		}
	}
	c := *ok
	c.SwitchMonitor, c.MacInput = false, "vga"
	if _, err := Validate(&c); err != nil {
		t.Errorf("关闭显示器切换时不应校验输入口: %v", err)
	}
	if FormatKey("abcde-fghij klmno-pqrst") != "ABCDE-FGHIJ-KLMNO-PQRST" || FormatKey("short") != "" || FormatKey("11111-11111-11111-11111") != "" {
		t.Error("FormatKey 不对")
	}
	if k := GenKey(); FormatKey(k) != k {
		t.Errorf("生成的密钥不合规: %q", k)
	}
}

// 升级：旧版留下的配置要胜过对端全新的默认配置；但新设备重启不能被误判成“旧版配置”。
func TestLegacyConfigMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	os.WriteFile(legacy, []byte(`{"pairing_key":"AAAAA-AAAAA-AAAAA-AAAAA","hotkey":"Ctrl+F9","emergency_hotkey":"Ctrl+Alt+Shift+F9","switch_monitor":true,"windows_input":"hdmi2","mac_input":"dp1","mod_ctrl":"control","mod_alt":"option","mod_win":"command","mouse_scale":1,"scroll_scale":1,"sync_clipboard":true,"max_auto_file_mb":1024,"port":47812}`), 0o600)
	c, created, err := LoadConfig(legacy)
	if err != nil || created || c.Rev != 1 || c.Hotkey != "Ctrl+F9" {
		t.Fatalf("旧配置迁移不对: %+v created=%v err=%v", c, created, err)
	}
	if c2, _, _ := LoadConfig(legacy); c2.Rev != 1 || c2.Schema != schemaVersion {
		t.Fatalf("迁移结果应已保存: %+v", c2)
	}

	fresh := filepath.Join(dir, "fresh.json")
	c, created, _ = LoadConfig(fresh)
	if !created || c.Rev != 0 {
		t.Fatalf("全新配置: %+v created=%v", c, created)
	}
	if c, created, _ = LoadConfig(fresh); created || c.Rev != 0 {
		t.Fatalf("新设备重启后 rev 不应被抬高: %+v created=%v", c, created)
	}
}

// 共享设置的两种表示必须字段一一对应，否则会出现“某项设置不同步”。
func TestSharedRoundTrip(t *testing.T) {
	a := DefaultConfig()
	a.Hotkey, a.MacDDCCommand, a.ScrollInvert, a.Rev = "Ctrl+F9", "x {input}", true, 42
	a.MouseScale, a.ScrollScale, a.MaxAutoFileMB, a.SyncClipboard = 2.5, 0.5, 7, false
	a.ModCtrl, a.ModWin, a.WindowsInput, a.MacInput, a.SwitchMonitor = "command", "control", "hdmi2", "dp2", false
	a.EmergencyHotkey, a.ModAlt = "Ctrl+F10", "option"
	b := DefaultConfig()
	b.PairingKey, b.PeerHost, b.Port = "KEEP", "1.2.3.4", 5
	got := b.WithShared(a.Shared())
	want := *a
	want.PairingKey, want.PeerHost, want.Port = "KEEP", "1.2.3.4", 5
	if *got != want {
		t.Fatalf("\n得到 %+v\n期望 %+v", *got, want)
	}
}

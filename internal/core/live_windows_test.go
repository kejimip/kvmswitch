package core

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"kvmswitch/internal/proto"
)

// 端到端测试：让一台“模拟 Mac”连接真实运行的 Windows 程序。
// 仅在设置了 KVM_LIVE_ADDR（如 127.0.0.1:47912）和 KVM_LIVE_KEY 时运行。
//
// 注意：这里只测试“Windows 被对方控制”这一个方向，并且只注入鼠标移动。
// 反方向会让真实程序吞掉这台电脑的物理键鼠，不适合在有人使用的机器上自动测试。
func TestLiveAgainstRunningWindows(t *testing.T) {
	addr, key := os.Getenv("KVM_LIVE_ADDR"), os.Getenv("KVM_LIVE_KEY")
	if addr == "" || key == "" {
		t.Skip("未设置 KVM_LIVE_ADDR / KVM_LIVE_KEY")
	}
	dir := t.TempDir()
	cfg := DefaultConfig() // 全新的默认配置（rev=0），应被对端已有的配置覆盖
	cfg.PairingKey, cfg.PeerHost = key, addr
	path := filepath.Join(dir, "config.json")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	f := &fake{os: "mac", name: "模拟 Mac", dir: filepath.Join(dir, "recv"), canControl: true}
	mac, _, err := New(f, Options{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := mac.Start(); err != nil {
		t.Fatal(err)
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("等待超时：%s（状态 %+v）", what, mac.State())
	}

	wait("连接并识别出 Windows", func() bool {
		s := mac.State()
		return s.Connected && s.PeerOS == "windows" && s.PeerName != "" && s.PeerCanControl
	})
	t.Logf("已连接：%s", mac.State().PeerName)

	wait("同步到 Windows 上已有的设置", func() bool {
		c := mac.Config()
		return c.Hotkey == "Ctrl+Alt+F9" && c.WindowsInput == "hdmi2" && !c.SwitchMonitor
	})
	if mac.Config().PairingKey != key {
		t.Fatal("同步不应改动配对密钥")
	}

	// 键鼠“插在 Mac 上”：按热键 -> 键鼠归 Windows，Mac 开始转发
	mac.Hotkey(false)
	wait("键鼠切到 Windows", func() bool { return mac.State().Active == ActivePeer })

	user32 := syscall.NewLazyDLL("user32.dll")
	getPos := func() (p struct{ X, Y int32 }) {
		user32.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&p)))
		return
	}
	before := getPos()
	mac.PushInput(InputEvent{Typ: proto.MsgMouseMove, A: 60, B: 35})
	wait("Windows 的光标随之移动", func() bool { p := getPos(); return p.X != before.X || p.Y != before.Y })
	moved := getPos()
	t.Logf("光标 %v -> %v", before, moved)
	user32.NewProc("SetCursorPos").Call(uintptr(before.X), uintptr(before.Y)) // 放回原处
}

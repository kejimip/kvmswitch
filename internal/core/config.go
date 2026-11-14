// Package core 是两端共用的对等核心：配置、热键、切换状态机、连接与剪贴板同步。
// 它不关心运行在哪个系统上，所有系统相关的能力通过 Platform 接口提供。
package core

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const Version = "1.3.12"

// Config 是一台设备上的全部配置。除 PairingKey、Port、PeerHost 外，其余为“共享设置”，
// 在任意一端修改后会自动同步到另一端。
const schemaVersion = 2

type MonitorChoice struct{ ID, Name string }

// No index fallback: a disconnected or ambiguous selection must never redirect a DDC write.
func MonitorIndex(ids []string, bound string) (int, error) {
	index := -1
	for i, id := range ids {
		if bound != "" && id != bound {
			continue
		}
		if index >= 0 {
			return -1, fmt.Errorf("存在多个候选显示器，请在本机绑定共享屏")
		}
		index = i
	}
	if index < 0 {
		return -1, fmt.Errorf("未找到共享屏，请检查连接并刷新绑定")
	}
	return index, nil
}

type Config struct {
	FileDir      string `json:"file_dir"`
	FileLimitMiB int64  `json:"file_limit_mib"`
	FilePaused   bool   `json:"file_paused"`
	MonitorID    string `json:"monitor_id"` // Local hardware path; never synchronized.
	Schema       int    `json:"schema"`     // 配置文件格式版本
	PairingKey   string `json:"pairing_key"`
	Port         int    `json:"port"`
	PeerHost     string `json:"peer_host,omitempty"` // 可选：手动指定对方 IP（仅主动发起连接的一端使用）

	Hotkey          string  `json:"hotkey"`
	EmergencyHotkey string  `json:"emergency_hotkey"`
	SwitchMonitor   bool    `json:"switch_monitor"`
	WindowsInput    string  `json:"windows_input"`
	MacInput        string  `json:"mac_input"`
	ModCtrl         string  `json:"mod_ctrl"`
	ModAlt          string  `json:"mod_alt"`
	ModWin          string  `json:"mod_win"`
	MouseScale      float64 `json:"mouse_scale"`
	ScrollScale     float64 `json:"scroll_scale"`
	ScrollInvert    bool    `json:"scroll_invert"`
	SyncClipboard   bool    `json:"sync_clipboard"`
	MaxAutoFileMB   int64   `json:"max_auto_file_mb"`
	MacDDCCommand   string  `json:"mac_ddc_command"`
	Rev             int64   `json:"rev"` // 共享设置最后一次修改的时间（毫秒），两端以较新的为准
}

// Shared 是需要在两端保持一致的那部分设置。
type Shared struct {
	Hotkey          string  `json:"hotkey"`
	EmergencyHotkey string  `json:"emergency_hotkey"`
	SwitchMonitor   bool    `json:"switch_monitor"`
	WindowsInput    string  `json:"windows_input"`
	MacInput        string  `json:"mac_input"`
	ModCtrl         string  `json:"mod_ctrl"`
	ModAlt          string  `json:"mod_alt"`
	ModWin          string  `json:"mod_win"`
	MouseScale      float64 `json:"mouse_scale"`
	ScrollScale     float64 `json:"scroll_scale"`
	ScrollInvert    bool    `json:"scroll_invert"`
	SyncClipboard   bool    `json:"sync_clipboard"`
	MaxAutoFileMB   int64   `json:"max_auto_file_mb"`
	MacDDCCommand   string  `json:"mac_ddc_command"`
	Rev             int64   `json:"rev"`
}

func (c *Config) Shared() Shared {
	return Shared{c.Hotkey, c.EmergencyHotkey, c.SwitchMonitor, c.WindowsInput, c.MacInput,
		c.ModCtrl, c.ModAlt, c.ModWin, c.MouseScale, c.ScrollScale, c.ScrollInvert,
		c.SyncClipboard, c.MaxAutoFileMB, c.MacDDCCommand, c.Rev}
}

// WithShared 返回一份套用了共享设置的新配置。
func (c *Config) WithShared(s Shared) *Config {
	n := *c
	n.Hotkey, n.EmergencyHotkey, n.SwitchMonitor = s.Hotkey, s.EmergencyHotkey, s.SwitchMonitor
	n.WindowsInput, n.MacInput = s.WindowsInput, s.MacInput
	n.ModCtrl, n.ModAlt, n.ModWin = s.ModCtrl, s.ModAlt, s.ModWin
	n.MouseScale, n.ScrollScale, n.ScrollInvert = s.MouseScale, s.ScrollScale, s.ScrollInvert
	n.SyncClipboard, n.MaxAutoFileMB, n.MacDDCCommand, n.Rev = s.SyncClipboard, s.MaxAutoFileMB, s.MacDDCCommand, s.Rev
	return &n
}

func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		FileDir: filepath.Join(home, "Downloads", "KVMSwitch"), FileLimitMiB: 10240,
		Schema:          schemaVersion,
		Port:            47812,
		Hotkey:          "Ctrl+F12",
		EmergencyHotkey: "Ctrl+Alt+Shift+F12",
		SwitchMonitor:   true,
		WindowsInput:    "hdmi1",
		MacInput:        "dp1",
		ModCtrl:         "control",
		ModAlt:          "option",
		ModWin:          "command",
		MouseScale:      1.0,
		ScrollScale:     1.0,
		SyncClipboard:   true,
		MaxAutoFileMB:   1024,
	}
}

func GenKey() string {
	b := make([]byte, 13)
	rand.Read(b)
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:20]
	return s[0:5] + "-" + s[5:10] + "-" + s[10:15] + "-" + s[15:20]
}

// NormKey 去掉分隔符并统一大小写，两端必须一致。
func NormKey(s string) []byte {
	s = strings.ToUpper(s)
	return []byte(strings.NewReplacer("-", "", " ", "").Replace(s))
}

// FormatKey 把用户输入的密钥整理成 XXXXX-XXXXX-XXXXX-XXXXX；格式不对返回空串。
func FormatKey(s string) string {
	n := string(NormKey(s))
	if len(n) != 20 {
		return ""
	}
	for _, r := range n {
		if !(r >= 'A' && r <= 'Z' || r >= '2' && r <= '7') {
			return ""
		}
	}
	return n[0:5] + "-" + n[5:10] + "-" + n[10:15] + "-" + n[15:20]
}

// LoadConfig 读取配置；不存在则创建。created 表示是首次运行。
func LoadConfig(path string) (cfg *Config, created bool, err error) {
	cfg = DefaultConfig()
	b, err := os.ReadFile(path)
	if err == nil {
		cfg.Schema = 0 // 文件里没有这个字段就说明是旧格式
		if err = json.Unmarshal(b, cfg); err != nil {
			return nil, false, fmt.Errorf("配置文件格式错误: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, false, err
	} else {
		created = true
	}
	migrated := false
	if !created && cfg.Schema < schemaVersion {
		// 1.1 及更早版本留下的配置：用户调过的设置应当胜过对端全新的默认设置（rev=0）。
		// 只在升级时做这一次，否则一台从未改过设置的新设备重启后也会被误判。
		if cfg.Rev == 0 {
			cfg.Rev = 1
		}
		migrated = true
	}
	cfg.Schema = schemaVersion
	if cfg.PairingKey == "" {
		cfg.PairingKey = GenKey()
		created = true
	}
	if cfg.Port == 0 {
		cfg.Port = 47812
	}
	if cfg.MouseScale <= 0 {
		cfg.MouseScale = 1
	}
	if cfg.ScrollScale <= 0 {
		cfg.ScrollScale = 1
	}
	if created || migrated {
		if err := SaveConfig(path, cfg); err != nil {
			return nil, false, err
		}
	}
	return cfg, created, nil
}

func SaveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

// ParseInput 把输入口名称或数字转换为 VCP 0x60 的取值。
func ParseInput(s string) (uint16, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "hdmi1":
		return 0x11, nil
	case "hdmi2":
		return 0x12, nil
	case "dp1":
		return 0x0F, nil
	case "dp2":
		return 0x10, nil
	}
	v, err := strconv.ParseUint(strings.TrimSpace(s), 0, 16)
	if err != nil {
		return 0, fmt.Errorf("无法识别的输入口 %q（可用 hdmi1/hdmi2/dp1/dp2 或数字，如 0x0f）", s)
	}
	return uint16(v), nil
}

// ---------------------------------------------------------------- 热键

// 热键统一用 Windows 虚拟键码表示；Mac 端采集到按键后先换算成虚拟键码再比较。
const (
	ModCtrl = 1 << iota
	ModAlt
	ModShift
	ModWin
)

type Hotkey struct {
	Mods uint32
	VK   uint32
}

// Pack 把热键压成一个整数，便于输入线程原子读取；VK 至少为 1，所以有效热键不会是 0。
func (h Hotkey) Pack() uint64 { return uint64(h.Mods)<<32 | uint64(h.VK) }

func Unpack(v uint64) Hotkey { return Hotkey{Mods: uint32(v >> 32), VK: uint32(v)} }

var keyNames = map[string]uint32{
	"scrolllock": 0x91, "pause": 0x13, "insert": 0x2D, "delete": 0x2E, "home": 0x24, "end": 0x23,
	"pageup": 0x21, "pagedown": 0x22, "printscreen": 0x2C, "capslock": 0x14, "esc": 0x1B, "escape": 0x1B,
	"space": 0x20, "tab": 0x09, "`": 0xC0, "numlock": 0x90,
}

var keyDisplay = map[uint32]string{
	0x91: "ScrollLock", 0x13: "Pause", 0x2D: "Insert", 0x2E: "Delete", 0x24: "Home", 0x23: "End",
	0x21: "PageUp", 0x22: "PageDown", 0x2C: "PrintScreen", 0x14: "CapsLock", 0x1B: "Esc",
	0x20: "Space", 0x09: "Tab", 0xC0: "`", 0x90: "NumLock",
}

func ParseHotkey(s string) (Hotkey, error) {
	var h Hotkey
	for _, tok := range strings.Split(s, "+") {
		t := strings.ToLower(strings.TrimSpace(tok))
		switch t {
		case "ctrl", "control":
			h.Mods |= ModCtrl
		case "alt":
			h.Mods |= ModAlt
		case "shift":
			h.Mods |= ModShift
		case "win", "windows", "super":
			h.Mods |= ModWin
		case "":
			return h, fmt.Errorf("热键 %q 格式错误", s)
		default:
			if h.VK != 0 {
				return h, fmt.Errorf("热键 %q 只能包含一个主键", s)
			}
			if v, ok := keyNames[t]; ok {
				h.VK = v
			} else if len(t) >= 2 && t[0] == 'f' {
				n, err := strconv.Atoi(t[1:])
				if err != nil || n < 1 || n > 24 {
					return h, fmt.Errorf("热键 %q 中无法识别的键 %q", s, tok)
				}
				h.VK = 0x70 + uint32(n) - 1
			} else if len(t) == 1 && (t[0] >= 'a' && t[0] <= 'z' || t[0] >= '0' && t[0] <= '9') {
				h.VK = uint32(strings.ToUpper(t)[0])
			} else {
				return h, fmt.Errorf("热键 %q 中无法识别的键 %q", s, tok)
			}
		}
	}
	if h.VK == 0 {
		return h, fmt.Errorf("热键 %q 缺少主键", s)
	}
	return h, nil
}

// FormatHotkey 是 ParseHotkey 的逆操作；遇到不支持的键返回空串。
func FormatHotkey(h Hotkey) string {
	var key string
	switch {
	case h.VK >= 0x70 && h.VK <= 0x87:
		key = fmt.Sprintf("F%d", h.VK-0x70+1)
	case h.VK >= 'A' && h.VK <= 'Z', h.VK >= '0' && h.VK <= '9':
		key = string(rune(h.VK))
	default:
		key = keyDisplay[h.VK]
	}
	if key == "" {
		return ""
	}
	var s string
	if h.Mods&ModCtrl != 0 {
		s += "Ctrl+"
	}
	if h.Mods&ModAlt != 0 {
		s += "Alt+"
	}
	if h.Mods&ModShift != 0 {
		s += "Shift+"
	}
	if h.Mods&ModWin != 0 {
		s += "Win+"
	}
	return s + key
}

// Parsed 是校验配置后得到的派生值。
type Parsed struct {
	Toggle, Emergency  Hotkey
	WinInput, MacInput uint16
}

func validMod(s string) bool { return s == "control" || s == "option" || s == "command" }

// needsModifier 报告该主键是否必须搭配 Ctrl/Alt/Win，避免把日常按键设成热键。
func needsModifier(vk uint32) bool {
	return !(vk >= 0x70 && vk <= 0x87 || vk == 0x91 || vk == 0x13) // F1-F24、ScrollLock、Pause 可单独使用
}

// Validate 检查配置并返回派生值；出错信息面向用户。
func Validate(cfg *Config) (p Parsed, err error) {
	if p.Toggle, err = ParseHotkey(cfg.Hotkey); err != nil {
		return p, err
	}
	if p.Emergency, err = ParseHotkey(cfg.EmergencyHotkey); err != nil {
		return p, err
	}
	for _, h := range []Hotkey{p.Toggle, p.Emergency} {
		if needsModifier(h.VK) && h.Mods&(ModCtrl|ModAlt|ModWin) == 0 {
			return p, fmt.Errorf("热键需要搭配 Ctrl、Alt 或 Win（功能键 F1-F24 可单独使用）")
		}
	}
	if p.Toggle == p.Emergency {
		return p, fmt.Errorf("切换热键和紧急热键不能相同")
	}
	if cfg.SwitchMonitor {
		if p.WinInput, err = ParseInput(cfg.WindowsInput); err != nil {
			return p, fmt.Errorf("Windows 输入口有误：%v", err)
		}
		if p.MacInput, err = ParseInput(cfg.MacInput); err != nil {
			return p, fmt.Errorf("Mac 输入口有误：%v", err)
		}
		if p.WinInput == p.MacInput {
			return p, fmt.Errorf("两台设备不能使用同一个显示器输入口")
		}
	}
	mods := map[string]bool{cfg.ModCtrl: true, cfg.ModAlt: true, cfg.ModWin: true}
	if !validMod(cfg.ModCtrl) || !validMod(cfg.ModAlt) || !validMod(cfg.ModWin) {
		return p, fmt.Errorf("修饰键对应只能是 control、option 或 command")
	}
	if len(mods) != 3 {
		return p, fmt.Errorf("Ctrl、Alt、Win 需要分别对应不同的 Mac 修饰键")
	}
	if cfg.MouseScale < 0.1 || cfg.MouseScale > 10 || cfg.ScrollScale < 0.1 || cfg.ScrollScale > 10 {
		return p, fmt.Errorf("速度倍率需在 0.1 到 10 之间")
	}
	if !filepath.IsAbs(cfg.FileDir) || cfg.FileLimitMiB < 1 || cfg.FileLimitMiB > 1<<20 {
		return p, fmt.Errorf("接收目录须为绝对路径，单批上限为 1–1048576 MiB")
	}
	if cfg.MaxAutoFileMB < 0 {
		return p, fmt.Errorf("文件大小上限不能为负数")
	}
	return p, nil
}

func nowMs() int64 { return time.Now().UnixMilli() }

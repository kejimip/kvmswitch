package main

// 键码换算。线路上统一使用 Windows 虚拟键码（VK），Mac 端在两个方向上做换算：
//   被控时：VK -> macOS 键码（kVK_*）
//   采集时：macOS 键码 -> VK

const (
	flagCaps    = 0x10000
	flagShift   = 0x20000
	flagControl = 0x40000
	flagOption  = 0x80000
	flagCommand = 0x100000
	modMask     = flagCaps | flagShift | flagControl | flagOption | flagCommand

	kcV        = 9
	kcCapsLock = 57
)

// modMap 是修饰键对应关系：PC 键盘的 Ctrl / Alt / Win 各对应 Mac 的哪个修饰键。
type modMap struct{ ctrl, alt, win string }

var defaultMods = modMap{"control", "option", "command"}

var vkMap = map[uint16]uint16{
	// 字母
	0x41: 0, 0x53: 1, 0x44: 2, 0x46: 3, 0x48: 4, 0x47: 5, 0x5A: 6, 0x58: 7, 0x43: 8, 0x56: 9,
	0x42: 11, 0x51: 12, 0x57: 13, 0x45: 14, 0x52: 15, 0x59: 16, 0x54: 17,
	0x4F: 31, 0x55: 32, 0x49: 34, 0x50: 35, 0x4C: 37, 0x4A: 38, 0x4B: 40, 0x4E: 45, 0x4D: 46,
	// 数字行
	0x31: 18, 0x32: 19, 0x33: 20, 0x34: 21, 0x36: 22, 0x35: 23, 0x39: 25, 0x37: 26, 0x38: 28, 0x30: 29,
	// 符号
	0xBB: 24, 0xBD: 27, 0xDD: 30, 0xDB: 33, 0xDE: 39, 0xBA: 41, 0xDC: 42, 0xBC: 43, 0xBF: 44, 0xBE: 47, 0xC0: 50,
	0xE2: 10, // ISO 键盘多出的那个键
	// 控制键
	0x0D: 36, 0x09: 48, 0x20: 49, 0x08: 51, 0x1B: 53, 0x14: kcCapsLock,
	// 导航
	0x24: 115, 0x21: 116, 0x2E: 117, 0x23: 119, 0x22: 121, 0x2D: 114,
	0x25: 123, 0x27: 124, 0x28: 125, 0x26: 126,
	// 功能键
	0x70: 122, 0x71: 120, 0x72: 99, 0x73: 118, 0x74: 96, 0x75: 97, 0x76: 98, 0x77: 100,
	0x78: 101, 0x79: 109, 0x7A: 103, 0x7B: 111,
	0x7C: 105, 0x7D: 107, 0x7E: 113, 0x7F: 106, 0x80: 64, 0x81: 79, 0x82: 80, 0x83: 90,
	0x2C: 105, 0x91: 107, 0x13: 113, // PrintScreen/ScrollLock/Pause -> F13/F14/F15
	// 小键盘
	0x60: 82, 0x61: 83, 0x62: 84, 0x63: 85, 0x64: 86, 0x65: 87, 0x66: 88, 0x67: 89, 0x68: 91, 0x69: 92,
	0x6A: 67, 0x6B: 69, 0x6D: 78, 0x6E: 65, 0x6F: 75, 0x90: 71,
	// Shift
	0xA0: 56, 0xA1: 60, 0x10: 56,
}

// revMap 是 vkMap 的逆映射（macOS 键码 -> VK）。
var revMap = func() map[uint16]uint16 {
	m := make(map[uint16]uint16, len(vkMap))
	for vk, code := range vkMap {
		m[code] = vk
	}
	// 多个 VK 对应同一个 Mac 键码时，固定取其中一个
	m[56], m[60] = 0xA0, 0xA1
	m[105], m[107], m[113] = 0x7C, 0x7D, 0x7E
	return m
}()

// 在 Windows 上需要带“扩展键”标志注入的 VK
var extendedVK = map[uint16]bool{
	0x21: true, 0x22: true, 0x23: true, 0x24: true, 0x25: true, 0x26: true, 0x27: true, 0x28: true,
	0x2D: true, 0x2E: true, 0xA3: true, 0xA5: true, 0x5B: true, 0x5C: true, 0x6F: true,
}

// modTarget 返回 Mac 修饰键的 (左, 右) 键码。
func modTarget(name string) (uint16, uint16) {
	switch name {
	case "command":
		return 55, 54
	case "option":
		return 58, 61
	default:
		return 59, 62 // control
	}
}

// mapKey：VK -> macOS 键码。
func mapKey(vk uint16, extended bool, m modMap) (uint16, bool) {
	pick := func(name string, right bool) (uint16, bool) {
		l, r := modTarget(name)
		if right {
			return r, true
		}
		return l, true
	}
	switch vk {
	case 0xA2, 0x11:
		return pick(m.ctrl, false)
	case 0xA3:
		return pick(m.ctrl, true)
	case 0xA4, 0x12:
		return pick(m.alt, false)
	case 0xA5:
		return pick(m.alt, true)
	case 0x5B:
		return pick(m.win, false)
	case 0x5C:
		return pick(m.win, true)
	case 0x0D:
		if extended {
			return 76, true // 小键盘回车
		}
	}
	k, ok := vkMap[vk]
	return k, ok
}

// macModName 返回某个 Mac 修饰键码属于哪一类，以及是否为右侧键。
func macModName(code uint16) (name string, right bool) {
	switch code {
	case 59:
		return "control", false
	case 62:
		return "control", true
	case 58:
		return "option", false
	case 61:
		return "option", true
	case 55:
		return "command", false
	case 54:
		return "command", true
	}
	return "", false
}

// unmapKey：macOS 键码 -> VK（mapKey 的逆操作）。
func unmapKey(code uint16, m modMap) (vk uint16, extended, ok bool) {
	if name, right := macModName(code); name != "" {
		var l, r uint16
		switch name {
		case m.ctrl:
			l, r = 0xA2, 0xA3
		case m.alt:
			l, r = 0xA4, 0xA5
		case m.win:
			l, r = 0x5B, 0x5C
		default:
			return 0, false, false
		}
		if right {
			return r, extendedVK[r], true
		}
		return l, extendedVK[l], true
	}
	if code == 76 {
		return 0x0D, true, true
	}
	vk, ok = revMap[code]
	return vk, extendedVK[vk], ok
}

func modFlagOf(code uint16) uint64 {
	switch code {
	case 55, 54:
		return flagCommand
	case 56, 60:
		return flagShift
	case 58, 61:
		return flagOption
	case 59, 62:
		return flagControl
	}
	return 0
}

// 各修饰键在事件标志里对应的“设备相关”位，用来判断 flagsChanged 是按下还是抬起
var deviceModBit = map[uint16]uint64{
	59: 0x1, 56: 0x2, 60: 0x4, 55: 0x8, 54: 0x10, 58: 0x20, 61: 0x40, 62: 0x2000,
}

// vkModBit 把修饰键 VK 归类为热键用的修饰位（与 core.ModCtrl 等一致）。
func vkModBit(vk uint16) uint32 {
	switch vk {
	case 0xA2, 0xA3:
		return 1 // Ctrl
	case 0xA4, 0xA5:
		return 2 // Alt
	case 0xA0, 0xA1:
		return 4 // Shift
	case 0x5B, 0x5C:
		return 8 // Win
	}
	return 0
}

// NX_KEYTYPE ↔ Windows 媒体 VK；这些不是普通的 macOS 键码。
var mediaKeys = map[uint16]uint16{0xAF: 0, 0xAE: 1, 0xAD: 7, 0xB3: 16, 0xB0: 17, 0xB1: 18}

func decodeMedia(data uint32) (uint16, bool, bool) {
	nx := uint16(data >> 16)
	phase := byte(data >> 8)
	if phase != 10 && phase != 11 {
		return 0, false, false
	}
	for vk, n := range mediaKeys {
		if nx == n {
			return vk, phase == 10, true
		}
	}
	return 0, false, false
}

package main

import "testing"

var allMods = []modMap{
	{"control", "option", "command"}, // 按键位对应
	{"command", "option", "control"}, // 沿用 Windows 习惯
	{"option", "command", "control"},
	{"option", "control", "command"},
	{"command", "control", "option"},
	{"control", "command", "option"},
}

// 被控方向（VK -> Mac）和采集方向（Mac -> VK）必须互为逆运算，
// 否则键鼠插在 Mac 上时按下的键到了 Windows 会变成别的键。
func TestKeymapRoundTrip(t *testing.T) {
	// 这些 VK 与另一个 VK 共用同一个 Mac 键码，反向换算时固定归到后者
	alias := map[uint16]uint16{0x10: 0xA0, 0x2C: 0x7C, 0x91: 0x7D, 0x13: 0x7E}
	for _, m := range allMods {
		for vk := range vkMap {
			code, ok := mapKey(vk, false, m)
			if !ok {
				t.Fatalf("VK %#x 无法换算", vk)
			}
			back, ext, ok := unmapKey(code, m)
			if !ok {
				t.Fatalf("Mac 键码 %d 无法换算回 VK", code)
			}
			want := vk
			if a, isAlias := alias[vk]; isAlias {
				want = a
			}
			if back != want {
				t.Errorf("%+v: VK %#x -> %d -> %#x", m, vk, code, back)
			}
			if ext != extendedVK[back] {
				t.Errorf("VK %#x 的扩展键标志不对", back)
			}
		}
	}
}

func TestMacKeycodesUnique(t *testing.T) {
	seen := map[uint16]uint16{}
	shared := map[uint16]bool{56: true, 105: true, 107: true, 113: true} // 有意共用的键码
	for vk, code := range vkMap {
		if prev, dup := seen[code]; dup && !shared[code] {
			t.Errorf("VK %#x 和 %#x 都映射到 Mac 键码 %d", prev, vk, code)
		}
		seen[code] = vk
	}
}

func TestModifierMapping(t *testing.T) {
	for _, m := range allMods {
		// 六个 PC 修饰键 -> 六个不同的 Mac 修饰键 -> 原样换算回来
		got := map[uint16]bool{}
		for _, vk := range []uint16{0xA2, 0xA3, 0xA4, 0xA5, 0x5B, 0x5C} {
			code, ok := mapKey(vk, false, m)
			if !ok || modFlagOf(code) == 0 || deviceModBit[code] == 0 {
				t.Fatalf("%+v: 修饰键 %#x -> %d 不是合法的 Mac 修饰键", m, vk, code)
			}
			got[code] = true
			back, _, ok := unmapKey(code, m)
			if !ok || back != vk {
				t.Errorf("%+v: %#x -> %d -> %#x", m, vk, code, back)
			}
		}
		if len(got) != 6 {
			t.Errorf("%+v: 六个修饰键没有映射到六个不同的键", m)
		}
	}
	// “沿用 Windows 习惯”：PC 的 Ctrl 就是 Mac 的 Command
	if c, _ := mapKey(0xA2, false, allMods[1]); modFlagOf(c) != flagCommand {
		t.Error("Ctrl 应映射为 Command")
	}
	// Mac 键盘上的 Command 在默认方案下相当于 Win 键，在 Windows 习惯方案下相当于 Ctrl
	if vk, _, _ := unmapKey(55, allMods[0]); vk != 0x5B {
		t.Errorf("默认方案下 Command 应为 Win，得到 %#x", vk)
	}
	if vk, _, _ := unmapKey(55, allMods[1]); vk != 0xA2 {
		t.Errorf("Windows 习惯方案下 Command 应为 Ctrl，得到 %#x", vk)
	}
}

func TestSpecialKeys(t *testing.T) {
	m := defaultMods
	if c, _ := mapKey(0x0D, true, m); c != 76 {
		t.Error("带扩展标志的回车应为小键盘回车")
	}
	if c, _ := mapKey(0x0D, false, m); c != 36 {
		t.Error("普通回车不对")
	}
	if vk, ext, ok := unmapKey(76, m); !ok || vk != 0x0D || !ext {
		t.Error("小键盘回车反向换算不对")
	}
	for _, code := range []uint16{123, 124, 125, 126} { // 方向键在 Windows 上必须带扩展标志
		if _, ext, ok := unmapKey(code, m); !ok || !ext {
			t.Errorf("方向键 %d 应带扩展标志", code)
		}
	}
	if _, _, ok := unmapKey(63, m); ok { // Fn 键没有对应
		t.Error("Fn 键不应有对应的 VK")
	}
	// 热键用的修饰位：左右两侧归为同一类
	if vkModBit(0xA2) != vkModBit(0xA3) || vkModBit(0xA2) == vkModBit(0xA4) || vkModBit(0x41) != 0 {
		t.Error("vkModBit 不对")
	}
}

func TestMediaKeyRoundTrip(t *testing.T) {
	for vk, nx := range mediaKeys {
		for _, phase := range []uint32{10, 11} {
			v, down, ok := decodeMedia(uint32(nx)<<16 | phase<<8 | 1)
			if !ok || v != vk || down != (phase == 10) {
				t.Fatal(vk, phase, v, down)
			}
		}
	}
	if _, _, ok := decodeMedia(0xffff0a00); ok {
		t.Fatal("unknown media key")
	}
}

func TestRemoteKeyboardFlags(t *testing.T) {
	const fn = 0x800000
	// Independent ANSI keycode expectations in A-Z and 0-9 order. A
	// round-trip alone cannot spot two keys swapped in both directions.
	for start, codes := range map[uint16][]uint16{
		0x41: {0, 11, 8, 2, 14, 3, 5, 4, 34, 38, 40, 37, 46, 45, 31, 35, 12, 15, 1, 17, 32, 9, 13, 7, 16, 6},
		0x30: {29, 18, 19, 20, 21, 23, 22, 26, 28, 25},
		0x70: {122, 120, 99, 118, 96, 97, 98, 100, 101, 109, 103, 111, 105, 107, 113, 106, 64, 79, 80, 90},
	} {
		for i, want := range codes {
			vk := start + uint16(i)
			code, ok := mapKey(vk, false, defaultMods)
			if !ok || code != want || remoteKeyFlags(code, 0) != 0 {
				t.Fatalf("VK %#x -> %d flags %#x", vk, code, remoteKeyFlags(code, 0))
			}
		}
	}
	for vk := range vkMap {
		code, _ := mapKey(vk, false, defaultMods)
		if remoteKeyFlags(code, 0)&fn != 0 {
			t.Fatal("unexpected Fn", vk)
		}
	}
	for _, code := range []uint16{123, 124, 125, 126, 76, 82, 92} {
		if remoteKeyFlags(code, flagShift) != flagNumericPad|flagShift {
			t.Fatal(code)
		}
	}
}
func TestModifierLifetime(t *testing.T) {
	m := remoteModifiers{}
	m.update(0xa2, 55, false)
	if m.flags(false) != 0 {
		t.Fatal("unmatched hotkey release")
	}
	m.update(0xa2, 55, true)
	m.update(0xa3, 54, true)
	if m.flags(false) != flagCommand|0x8|0x10 {
		t.Fatal("both Command keys")
	}
	if c, ok := m.update(0xa2, 59, false); !ok || c != 55 {
		t.Fatal("released a different key after layout change")
	}
	if m.flags(false) != flagCommand|0x10 {
		t.Fatal("released both sides")
	}
	m.update(0xa3, 54, false)
	if m.flags(true) != flagCaps || remoteKeyFlags(12, m.flags(false)) != 0 || remoteKeyFlags(4, m.flags(false)) != 0 {
		t.Fatal("residual modifier on Q/H")
	}
}

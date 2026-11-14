package main

import "testing"

func TestDriverVolumeFilter(t *testing.T) {
	for vk := uint32(0); vk < 256; vk++ {
		for _, flags := range []uint32{0, llkhfInjected, llkhfInjected | llkhfUp | llkhfExtended} {
			for _, tag := range []uintptr{0, 123, kvmKeyTag} {
				want := flags&llkhfInjected != 0 && tag != kvmKeyTag && (vk == 0xAD || vk == 0xAE || vk == 0xAF)
				if driverVolume(&kbdllhook{VkCode: vk, Flags: flags, ExtraInfo: tag}) != want {
					t.Fatalf("VK=%x flags=%x tag=%x want %t", vk, flags, tag, want)
				}
			}
		}
	}
}

package main

import (
	"sync"

	"github.com/ebitengine/purego"
)

// CoreAudio HAL: default output and its main-element volume/mute properties.
// Resolve the default output on each turn: it may have changed to headphones.
var (
	audioGet func(uint32, *audioAddress, uint32, uintptr, *uint32, *uint32) int32
	audioSet func(uint32, *audioAddress, uint32, uintptr, uint32, *uint32) int32

	audioReady = sync.OnceValue(func() bool {
		h, err := purego.Dlopen("/System/Library/Frameworks/CoreAudio.framework/CoreAudio", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			return false
		}
		get, errGet := purego.Dlsym(h, "AudioObjectGetPropertyData")
		set, errSet := purego.Dlsym(h, "AudioObjectSetPropertyData")
		if errGet != nil || errSet != nil {
			purego.Dlclose(h)
			return false
		}
		purego.RegisterFunc(&audioGet, get)
		purego.RegisterFunc(&audioSet, set)
		return true
	})
)

func audioRead(device uint32, prop *audioAddress) (uint32, bool) {
	var v uint32
	n := uint32(4)
	ok := audioGet(device, prop, 0, 0, &n, &v) == 0 && n == 4
	return v, ok
}

func audioWrite(device uint32, prop *audioAddress, v uint32) bool {
	return audioSet(device, prop, 0, 0, 4, &v) == 0
}

func adjustOutputVolume(nx uint16) bool {
	if !audioReady() {
		return false
	}
	dev, ok := audioRead(1, &defaultOutput) // kAudioObjectSystemObject
	if !ok || dev == 0 {
		return false
	}
	return adjustVolume(nx, func(p *audioAddress) (uint32, bool) { return audioRead(dev, p) },
		func(p *audioAddress, value uint32) bool { return audioWrite(dev, p, value) })
}

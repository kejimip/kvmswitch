package main

import "math"

// A down/up pair must use the same route. A successful direct volume
// adjustment doesn't generate a key that needs releasing on disconnect.
type mediaPresses map[uint16]bool // NX key -> handled directly rather than posted

const (
	nxVolumeUp   = 0
	nxVolumeDown = 1
	nxMute       = 7
)

func (m mediaPresses) key(nx uint16, down bool, adjust func(uint16) bool, post func(uint16, bool, bool)) {
	direct, held := m[nx]
	if !down {
		if held && !direct {
			post(nx, false, false)
		}
		delete(m, nx)
		return
	}
	if !held {
		direct = (nx == nxVolumeUp || nx == nxVolumeDown || nx == nxMute) && adjust(nx)
		m[nx] = direct
	} else if direct && nx != nxMute {
		adjust(nx)
	}
	if !direct {
		post(nx, true, held)
	}
}

func (m mediaPresses) release(post func(uint16, bool, bool)) {
	for nx, direct := range m {
		if !direct {
			post(nx, false, false)
		}
		delete(m, nx)
	}
}

type audioAddress struct{ selector, scope, element uint32 }

var (
	defaultOutput = audioAddress{0x644f7574, 0x676c6f62, 0} // dOut, glob
	outputVolume  = audioAddress{0x766f6c6d, 0x6f757470, 0} // volm, outp
	outputMute    = audioAddress{0x6d757465, 0x6f757470, 0} // mute, outp
)

func adjustVolume(nx uint16, read func(*audioAddress) (uint32, bool), write func(*audioAddress, uint32) bool) bool {
	if nx == nxMute {
		muted, ok := read(&outputMute)
		if !ok {
			return false
		}
		var next uint32
		if muted == 0 {
			next = 1
		}
		return write(&outputMute, next)
	}
	bits, ok := read(&outputVolume)
	if !ok {
		return false
	}
	v := math.Float32frombits(bits)
	if !(v >= 0 && v <= 1) {
		return false
	}
	if nx == nxVolumeDown {
		v = max(0, v-1.0/16)
	} else {
		v = min(1, v+1.0/16)
	}
	if !write(&outputVolume, math.Float32bits(v)) {
		return false // e.g. fixed-level HDMI; let the normal system media key run
	}
	if nx == nxVolumeUp {
		if mute, ok := read(&outputMute); ok && mute != 0 {
			write(&outputMute, 0)
		}
	}
	return true
}

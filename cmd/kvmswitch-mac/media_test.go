package main

import (
	"math"
	"reflect"
	"testing"
)

func TestMediaPressRoute(t *testing.T) {
	for _, tc := range []struct {
		key      uint16
		writable bool
		adjusts  int
		posts    []string
	}{
		{nxVolumeUp, true, 2, nil}, {nxVolumeDown, true, 2, nil}, {nxMute, true, 1, nil},
		{nxVolumeUp, false, 1, []string{"down", "repeat", "up"}},
		{16, true, 0, []string{"down", "repeat", "up"}},
	} {
		for _, disconnect := range []bool{true, false} {
			m := mediaPresses{}
			var posted []string
			n := 0
			adjust := func(uint16) bool { n++; return tc.writable }
			post := func(_ uint16, down, repeat bool) {
				p := "up"
				if repeat {
					p = "repeat"
				} else if down {
					p = "down"
				}
				posted = append(posted, p)
			}
			m.key(tc.key, true, adjust, post)
			m.key(tc.key, true, adjust, post)
			if disconnect {
				m.release(post)
			}
			m.key(tc.key, false, adjust, post)
			if n != tc.adjusts || !reflect.DeepEqual(posted, tc.posts) || len(m) != 0 {
				t.Fatalf("case %+v disconnect=%v: adjust=%d events=%v held=%v", tc, disconnect, n, posted, m)
			}
		}
	}
}

func TestOutputVolume(t *testing.T) {
	values := map[*audioAddress]uint32{&outputVolume: math.Float32bits(0.5), &outputMute: 1}
	read := func(p *audioAddress) (uint32, bool) { v, ok := values[p]; return v, ok }
	write := func(p *audioAddress, v uint32) bool { values[p] = v; return true }
	check := func(nx uint16, want float32, mute uint32) {
		t.Helper()
		if !adjustVolume(nx, read, write) || math.Float32frombits(values[&outputVolume]) != want || values[&outputMute] != mute {
			t.Fatalf("nx=%d values=%v", nx, values)
		}
	}
	check(nxVolumeUp, 0.5625, 0)
	check(nxVolumeDown, 0.5, 0)
	check(nxMute, 0.5, 1)
	check(nxMute, 0.5, 0)
	values[&outputVolume] = math.Float32bits(1)
	check(nxVolumeUp, 1, 0)
	values[&outputVolume] = 0
	check(nxVolumeDown, 0, 0)
	if adjustVolume(nxVolumeUp, read, func(*audioAddress, uint32) bool { return false }) {
		t.Fatal("read-only output should fall back")
	}
	delete(values, &outputVolume)
	if adjustVolume(nxVolumeUp, read, write) {
		t.Fatal("missing control should fall back")
	}
}

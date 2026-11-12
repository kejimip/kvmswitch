package main

import (
	"errors"
	"fmt"
	"kvmswitch/internal/core"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 通过 DDC/CI 读写显示器的 VCP 0x60（输入源选择）。

const vcpInput = 0x60

type physMonitor struct {
	Handle uintptr
	Desc   [128]uint16
}

var (
	ddcMu    sync.Mutex
	enumOnce sync.Once
	enumCB   uintptr
	hmons    []uintptr
)

// enumPhysicalMonitors 需在持有 ddcMu 时调用。
type displayHandle struct {
	physMonitor
	id string
}

func devicePath(h uintptr) string {
	var m struct {
		Size  uint32
		Rects [8]int32
		Flags uint32
		Name  [32]uint16
	}
	m.Size = uint32(unsafe.Sizeof(m))
	if r, _, _ := user32.NewProc("GetMonitorInfoW").Call(h, uintptr(unsafe.Pointer(&m))); r == 0 {
		return ""
	}
	var d struct {
		Size    uint32
		Name    [32]uint16
		Desc    [128]uint16
		Flags   uint32
		ID, Key [128]uint16
	}
	// An HMONITOR with multiple active children (clone mode) is ambiguous.
	id := ""
	for i := uintptr(0); ; i++ {
		d.Size = uint32(unsafe.Sizeof(d))
		r, _, _ := user32.NewProc("EnumDisplayDevicesW").Call(uintptr(unsafe.Pointer(&m.Name[0])), i, uintptr(unsafe.Pointer(&d)), 1)
		if r == 0 {
			break
		}
		if d.Flags&1 != 0 {
			if id != "" {
				return ""
			}
			id = windows.UTF16ToString(d.ID[:])
		}
	}
	return id
}
func enumPhysicalMonitors() []displayHandle {
	// NewCallback 创建的回调无法释放，所以只创建一次
	enumOnce.Do(func() {
		enumCB = windows.NewCallback(func(h, hdc, rect, data uintptr) uintptr {
			hmons = append(hmons, h)
			return 1
		})
	})
	hmons = hmons[:0]
	pEnumDisplayMonitors.Call(0, 0, enumCB, 0)
	var out []displayHandle
	for _, h := range hmons {
		var n uint32
		if r, _, _ := pGetNumberOfPhysicalMonitors.Call(h, uintptr(unsafe.Pointer(&n))); r == 0 || n == 0 {
			continue
		}
		arr := make([]physMonitor, n)
		if r, _, _ := pGetPhysicalMonitors.Call(h, uintptr(n), uintptr(unsafe.Pointer(&arr[0]))); r == 0 {
			continue
		}
		id := ""
		if len(arr) == 1 {
			id = devicePath(h)
		}
		for _, p := range arr {
			out = append(out, displayHandle{p, id})
		}
	}
	return out
}

func destroyMonitors(ms []displayHandle) {
	for i := range ms {
		pDestroyPhysicalMonitors.Call(1, uintptr(unsafe.Pointer(&ms[i])))
	}
}

func getVCP(h uintptr, code byte) (cur, max uint32, ok bool) {
	r, _, _ := pGetVCPFeature.Call(h, uintptr(code), 0, uintptr(unsafe.Pointer(&cur)), uintptr(unsafe.Pointer(&max)))
	return cur, max, r != 0
}

// ddcSetInput 把所有支持 DDC 输入源切换的显示器切到指定输入口。
func selectDisplay(ms []displayHandle, id string) (displayHandle, error) {
	var candidates []displayHandle
	var ids []string
	for _, m := range ms {
		if _, _, ok := getVCP(m.Handle, vcpInput); ok {
			candidates = append(candidates, m)
			ids = append(ids, m.id)
		}
	}
	i, err := core.MonitorIndex(ids, id)
	if err != nil {
		return displayHandle{}, err
	}
	return candidates[i], nil
}
func (*winPlatform) MonitorChoices() []core.MonitorChoice {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	ms := enumPhysicalMonitors()
	defer destroyMonitors(ms)
	var out []core.MonitorChoice
	for _, m := range ms {
		if _, _, ok := getVCP(m.Handle, vcpInput); ok && m.id != "" {
			out = append(out, core.MonitorChoice{ID: m.id, Name: windows.UTF16ToString(m.Desc[:]) + " · " + m.id})
		}
	}
	return out
}
func ddcSetInput(v uint16, id string) error {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	ms := enumPhysicalMonitors()
	defer destroyMonitors(ms)
	m, err := selectDisplay(ms, id)
	if err != nil {
		return err
	}
	r, _, err := pSetVCPFeature.Call(m.Handle, vcpInput, uintptr(v))
	if r == 0 {
		return fmt.Errorf("显示器拒绝切换: %w", err)
	}
	return nil
}

// ddcReport 返回各显示器的 DDC 信息，用于排查。
func ddcReport() string {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	ms := enumPhysicalMonitors()
	defer destroyMonitors(ms)
	var sb strings.Builder
	if len(ms) == 0 {
		sb.WriteString("没有找到显示器\r\n")
	}
	for i, m := range ms {
		fmt.Fprintf(&sb, "显示器 %d: %s\r\n", i+1, windows.UTF16ToString(m.Desc[:]))
		if cur, _, ok := getVCP(m.Handle, vcpInput); ok {
			fmt.Fprintf(&sb, "  当前输入源读数: 0x%02X（部分显示器读数不准，属正常）\r\n", cur&0xFF)
		} else {
			sb.WriteString("  不支持 DDC 输入源读取\r\n")
			continue
		}
		var n uint32
		if r, _, _ := pGetCapsLen.Call(m.Handle, uintptr(unsafe.Pointer(&n))); r != 0 && n > 0 {
			buf := make([]byte, n)
			if r, _, _ := pCapsRequest.Call(m.Handle, uintptr(unsafe.Pointer(&buf[0])), uintptr(n)); r != 0 {
				caps := strings.TrimRight(string(buf), "\x00")
				if i := strings.Index(caps, "60("); i >= 0 {
					if j := strings.IndexByte(caps[i:], ')'); j > 0 {
						fmt.Fprintf(&sb, "  支持的输入源取值: %s\r\n", strings.TrimSpace(caps[i+3:i+j]))
						sb.WriteString("  （11=hdmi1  12=hdmi2  0F=dp1  10=dp2）\r\n")
					}
				}
			}
		}
	}
	return sb.String()
}

func (*winPlatform) CurrentInput(id string) (uint16, error) {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	ms := enumPhysicalMonitors()
	defer destroyMonitors(ms)
	m, err := selectDisplay(ms, id)
	if err != nil {
		return 0, err
	}
	v, _, ok := getVCP(m.Handle, vcpInput)
	if !ok || v == 0 || v > 65535 {
		return 0, errors.New("无法读取当前输入源")
	}
	return uint16(v), nil
}

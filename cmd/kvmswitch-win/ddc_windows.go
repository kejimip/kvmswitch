package main

import (
	"errors"
	"fmt"
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
func enumPhysicalMonitors() []physMonitor {
	// NewCallback 创建的回调无法释放，所以只创建一次
	enumOnce.Do(func() {
		enumCB = windows.NewCallback(func(h, hdc, rect, data uintptr) uintptr {
			hmons = append(hmons, h)
			return 1
		})
	})
	hmons = hmons[:0]
	pEnumDisplayMonitors.Call(0, 0, enumCB, 0)
	var out []physMonitor
	for _, h := range hmons {
		var n uint32
		if r, _, _ := pGetNumberOfPhysicalMonitors.Call(h, uintptr(unsafe.Pointer(&n))); r == 0 || n == 0 {
			continue
		}
		arr := make([]physMonitor, n)
		if r, _, _ := pGetPhysicalMonitors.Call(h, uintptr(n), uintptr(unsafe.Pointer(&arr[0]))); r == 0 {
			continue
		}
		out = append(out, arr...)
	}
	return out
}

func destroyMonitors(ms []physMonitor) {
	for i := range ms {
		pDestroyPhysicalMonitors.Call(1, uintptr(unsafe.Pointer(&ms[i])))
	}
}

func getVCP(h uintptr, code byte) (cur, max uint32, ok bool) {
	r, _, _ := pGetVCPFeature.Call(h, uintptr(code), 0, uintptr(unsafe.Pointer(&cur)), uintptr(unsafe.Pointer(&max)))
	return cur, max, r != 0
}

// ddcSetInput 把所有支持 DDC 输入源切换的显示器切到指定输入口。
func ddcSetInput(v uint16) error {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	ms := enumPhysicalMonitors()
	defer destroyMonitors(ms)
	if len(ms) == 0 {
		return errors.New("没有找到显示器")
	}
	okCount := 0
	var lastErr error
	for _, m := range ms {
		if _, _, ok := getVCP(m.Handle, vcpInput); !ok {
			continue // 笔记本内屏、虚拟显示器等不支持 DDC
		}
		r, _, err := pSetVCPFeature.Call(m.Handle, vcpInput, uintptr(v))
		if r != 0 {
			okCount++
		} else {
			lastErr = err
		}
	}
	if okCount == 0 {
		if lastErr != nil {
			return fmt.Errorf("显示器拒绝了切换指令: %v", lastErr)
		}
		return errors.New("没有支持 DDC/CI 输入源切换的显示器（请确认显示器菜单里的 DDC/CI 已开启）")
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

func (*winPlatform) CurrentInput() (uint16, error) {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	ms := enumPhysicalMonitors()
	defer destroyMonitors(ms)
	var found int
	var val uint32
	for _, m := range ms {
		if v, _, ok := getVCP(m.Handle, vcpInput); ok {
			found++
			val = v
		}
	}
	if found != 1 || val == 0 || val > 0xffff {
		return 0, errors.New("未能唯一确定当前显示器和输入源，请使用手动设置")
	}
	return uint16(val), nil
}

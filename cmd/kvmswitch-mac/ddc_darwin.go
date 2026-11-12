package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Apple 芯片上通过 IOAVService 的 I2C 接口向外接显示器发送 DDC/CI 指令，
// 用来把显示器切回 Windows 的输入口。

var (
	ddcOnce    sync.Once
	ddcLoadErr error

	ioRegistryGetRootEntry          func(port uint32) uint32
	ioRegistryEntryCreateIterator   func(entry uint32, plane string, opts uint32, iter *uint32) int32
	ioIteratorNext                  func(iter uint32) uint32
	ioRegistryEntryGetName          func(entry uint32, name *byte) int32
	ioObjectRelease                 func(obj uint32) int32
	ioRegistryEntryCreateCFProperty func(entry uint32, key uintptr, alloc uintptr, opts uint32) uintptr
	ioAVServiceCreateWithService    func(alloc uintptr, service uint32) uintptr
	ioAVServiceWriteI2C             func(svc uintptr, chip uint32, dataAddr uint32, buf *byte, n uint32) int32
)

func loadDDC() error {
	ddcOnce.Do(func() {
		iokit, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_GLOBAL|purego.RTLD_NOW)
		if err != nil {
			ddcLoadErr = err
			return
		}
		bind := func(fptr any, name string) {
			if ddcLoadErr != nil {
				return
			}
			addr, err := purego.Dlsym(iokit, name)
			if err != nil || addr == 0 {
				// 私有符号在个别系统版本里位于 CoreDisplay
				if cd, e := purego.Dlopen("/System/Library/Frameworks/CoreDisplay.framework/CoreDisplay", purego.RTLD_GLOBAL|purego.RTLD_NOW); e == nil {
					addr, err = purego.Dlsym(cd, name)
				}
			}
			if err != nil || addr == 0 {
				ddcLoadErr = fmt.Errorf("系统中找不到 %s，无法使用内置的显示器切换", name)
				return
			}
			purego.RegisterFunc(fptr, addr)
		}
		bind(&ioRegistryGetRootEntry, "IORegistryGetRootEntry")
		bind(&ioRegistryEntryCreateIterator, "IORegistryEntryCreateIterator")
		bind(&ioIteratorNext, "IOIteratorNext")
		bind(&ioRegistryEntryGetName, "IORegistryEntryGetName")
		bind(&ioObjectRelease, "IOObjectRelease")
		bind(&ioRegistryEntryCreateCFProperty, "IORegistryEntryCreateCFProperty")
		bind(&ioAVServiceCreateWithService, "IOAVServiceCreateWithService")
		bind(&ioAVServiceWriteI2C, "IOAVServiceWriteI2C")
	})
	return ddcLoadErr
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// ddcProbe 报告本机能否向外接显示器发送指令，供面板的“显示器信息”使用。
func ddcProbe(externalCmd string) string {
	if strings.TrimSpace(externalCmd) != "" {
		return "已配置外部切换命令，将使用它来切换显示器：\n  " + externalCmd
	}
	if err := loadDDC(); err != nil {
		return "本机无法使用内置的显示器切换：" + err.Error()
	}
	n := 0
	var iterErr error
	withPool(func() {
		var iter uint32
		if kr := ioRegistryEntryCreateIterator(ioRegistryGetRootEntry(0), "IOService", 1, &iter); kr != 0 {
			iterErr = fmt.Errorf("遍历 IORegistry 失败 (%d)", kr)
			return
		}
		defer ioObjectRelease(iter)
		name := make([]byte, 128)
		for {
			svc := ioIteratorNext(iter)
			if svc == 0 {
				break
			}
			for i := range name {
				name[i] = 0
			}
			ioRegistryEntryGetName(svc, &name[0])
			if cString(name) == "DCPAVServiceProxy" {
				if av := ioAVServiceCreateWithService(0, svc); av != 0 {
					n++
					cfRelease(av)
				}
			}
			ioObjectRelease(svc)
		}
	})
	if iterErr != nil {
		return iterErr.Error()
	}
	if n == 0 {
		return "没有找到可以发送切换指令的显示器接口。\n请尝试用 USB-C/雷雳口连接显示器，或在“高级”里填写外部切换命令。"
	}
	return fmt.Sprintf("找到 %d 个可以发送切换指令的显示器接口。\n是否真的生效，请用上面的“测试切换”确认。", n)
}

// ddcSetInput 把所有外接显示器切到指定输入口（VCP 0x60）。
// 若配置了外部命令，则改为执行该命令（其中 {input} 会被替换为十进制取值）。
func ddcSetInput(v uint16, externalCmd string) error {
	if strings.TrimSpace(externalCmd) != "" {
		cmd := strings.ReplaceAll(externalCmd, "{input}", strconv.Itoa(int(v)))
		out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
		if err != nil {
			return fmt.Errorf("执行 %q 失败: %v %s", cmd, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := loadDDC(); err != nil {
		return err
	}
	var result error
	withPool(func() {
		var iter uint32
		root := ioRegistryGetRootEntry(0)
		if kr := ioRegistryEntryCreateIterator(root, "IOService", 1 /* 递归 */, &iter); kr != 0 {
			result = fmt.Errorf("遍历 IORegistry 失败 (%d)", kr)
			return
		}
		defer ioObjectRelease(iter)

		// DDC/CI “设置 VCP” 报文
		pkt := []byte{0x84, 0x03, 0x60, byte(v >> 8), byte(v), 0}
		sum := byte(0x6E ^ 0x51)
		for _, b := range pkt[:5] {
			sum ^= b
		}
		pkt[5] = sum

		found, okCount := 0, 0
		var lastErr int32
		name := make([]byte, 128)
		for {
			svc := ioIteratorNext(iter)
			if svc == 0 {
				break
			}
			for i := range name {
				name[i] = 0
			}
			ioRegistryEntryGetName(svc, &name[0])
			if cString(name) == "DCPAVServiceProxy" {
				external := true
				if loc := ioRegistryEntryCreateCFProperty(svc, uintptr(nsString("Location")), 0, 0); loc != 0 {
					if objc.Send[bool](objc.ID(loc), sel("isKindOfClass:"), objc.GetClass("NSString")) {
						external = goString(objc.ID(loc)) != "Embedded"
					}
					cfRelease(loc)
				}
				if external {
					if av := ioAVServiceCreateWithService(0, svc); av != 0 {
						found++
						sent := false
						for i := 0; i < 2; i++ {
							time.Sleep(40 * time.Millisecond)
							if kr := ioAVServiceWriteI2C(av, 0x37, 0x51, (*byte)(unsafe.Pointer(&pkt[0])), uint32(len(pkt))); kr == 0 {
								sent = true
							} else {
								lastErr = kr
							}
						}
						if sent {
							okCount++
						}
						cfRelease(av)
					}
				}
			}
			ioObjectRelease(svc)
		}
		switch {
		case found == 0:
			result = errors.New("没有找到可发送 DDC 指令的外接显示器（请尝试换用 USB-C/雷雳口连接显示器）")
		case okCount == 0:
			result = fmt.Errorf("显示器未接受 DDC 指令 (0x%x)（请尝试换用 USB-C/雷雳口连接显示器）", uint32(lastErr))
		}
	})
	return result
}

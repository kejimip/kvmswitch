package main

import (
	"errors"
	"fmt"
	"kvmswitch/internal/core"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Apple 芯片上通过 IOAVService 的 I2C 接口向外接显示器发送 DDC/CI 指令，
// 用来把显示器切回 Windows 的输入口。

var (
	ddcOnce    sync.Once
	ddcLoadErr error

	ioRegistryEntryGetPath          func(uint32, string, *byte) int32
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
		bind(&ioRegistryEntryGetPath, "IORegistryEntryGetPath")
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

// Select by IOService connector path, not enumeration order.
type macDisplay struct {
	path string
	av   uintptr
}

func withDisplays(f func([]macDisplay) error) error {
	if err := loadDDC(); err != nil {
		return err
	}
	var result error
	withPool(func() {
		var iter uint32
		root := ioRegistryGetRootEntry(0)
		defer ioObjectRelease(root)
		if kr := ioRegistryEntryCreateIterator(root, "IOService", 1, &iter); kr != 0 {
			result = fmt.Errorf("遍历 IORegistry 失败 (%d)", kr)
			return
		}
		defer ioObjectRelease(iter)
		var all []macDisplay
		for {
			svc := ioIteratorNext(iter)
			if svc == 0 {
				break
			}
			func() {
				defer ioObjectRelease(svc)
				var name [128]byte
				ioRegistryEntryGetName(svc, &name[0])
				if cString(name[:]) != "DCPAVServiceProxy" {
					return
				}
				if loc := ioRegistryEntryCreateCFProperty(svc, uintptr(nsString("Location")), 0, 0); loc != 0 {
					defer cfRelease(loc)
					if objc.Send[bool](objc.ID(loc), sel("isKindOfClass:"), objc.GetClass("NSString")) && goString(objc.ID(loc)) == "Embedded" {
						return
					}
				}
				var path [512]byte
				if ioRegistryEntryGetPath(svc, "IOService", &path[0]) != 0 {
					return
				}
				if av := ioAVServiceCreateWithService(0, svc); av != 0 {
					all = append(all, macDisplay{cString(path[:]), av})
				}
			}()
		}
		defer func() {
			for _, d := range all {
				cfRelease(d.av)
			}
		}()
		result = f(all)
	})
	return result
}
func (*macPlatform) MonitorChoices() []core.MonitorChoice {
	var out []core.MonitorChoice
	withDisplays(func(ds []macDisplay) error {
		for _, d := range ds {
			out = append(out, core.MonitorChoice{ID: d.path, Name: d.path})
		}
		return nil
	})
	return out
}
func ddcSetInput(v uint16, externalCmd, id string) error {
	if strings.TrimSpace(externalCmd) != "" && id == "" {
		// Custom commands cannot necessarily address a particular panel. Never invoke one on an ambiguous topology.
		if err := withDisplays(func(ds []macDisplay) error {
			if len(ds) > 1 {
				return errors.New("多屏时请绑定共享屏，禁用未定向的外部命令")
			}
			return nil
		}); err != nil {
			return err
		}
		out, err := exec.Command("/bin/sh", "-c", strings.ReplaceAll(externalCmd, "{input}", strconv.Itoa(int(v)))).CombinedOutput()
		if err != nil {
			return fmt.Errorf("切换失败：%w %s", err, out)
		}
		return nil
	}
	return withDisplays(func(ds []macDisplay) error {
		ids := make([]string, len(ds))
		for i, d := range ds {
			ids[i] = d.path
		}
		index, err := core.MonitorIndex(ids, id)
		if err != nil {
			return err
		}
		av := ds[index].av
		pkt := []byte{0x84, 3, 0x60, byte(v >> 8), byte(v), 0}
		sum := byte(0x6e ^ 0x51)
		for _, b := range pkt[:5] {
			sum ^= b
		}
		pkt[5] = sum
		var kr int32
		for i := 0; i < 2; i++ {
			time.Sleep(40 * time.Millisecond)
			kr = ioAVServiceWriteI2C(av, 0x37, 0x51, &pkt[0], uint32(len(pkt)))
			if kr == 0 {
				return nil
			}
		}
		return fmt.Errorf("显示器未接受 DDC 指令 (0x%x)", uint32(kr))
	})
}

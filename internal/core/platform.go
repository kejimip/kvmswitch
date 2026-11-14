package core

import (
	"errors"
	"sync/atomic"
	"time"

	"kvmswitch/internal/xfer"
)

// ErrClipChanged means a newer local copy won before the received item was published.
var ErrClipChanged = errors.New("本机已复制新内容，未覆盖剪贴板")

// Platform 是核心对操作系统的全部要求。Windows 和 Mac 各实现一份。
// 两个方向的能力都必须具备：既能采集本机键鼠转发出去，也能接受对方的键鼠注入本机。
type Platform interface {
	OS() string // "windows" 或 "mac"
	Hostname() string
	RecvDir() string // 存放收到的文件

	// 本机键鼠采集。on 为 true 时吞掉本机的物理输入并交给核心转发（经 Core.PushInput）。
	SetForward(on bool)

	// 被对方控制。
	CanControl() bool        // 本机是否允许被注入输入（Mac 需要“辅助功能”权限）
	SetControlled(on bool)   // on：开始接受注入；off：松开所有仍按着的键
	ApplyTuning(cfg *Config) // 修饰键对应、速度倍率等
	InjectKey(vk uint16, flags byte) (paste bool)
	InjectMove(dx, dy int32)
	InjectButton(btn int, down bool)
	InjectWheel(dy, dx int32)

	// 显示器
	SetMonitorInput(v uint16, cfg *Config) error

	// 剪贴板
	ClipChange() uint64 // 每次剪贴板变化都会改变的计数
	ReadClip() (xfer.Clip, bool)
	WriteClip(kind string, data []byte, paths []string, expected uint64) (change uint64, err error)

	// 交互
	Confirm(msg string) bool // 弹窗询问，60 秒不答视为否
	Notify(title, text string)
	Beep()
	OnState(s State) // 状态变化时更新托盘/菜单栏图标
	KeepAwake(on bool)
}

// 当前由谁接收键鼠
const (
	ActiveNone = "none" // 尚未切换过，或未连接：两台设备各自独立
	ActiveSelf = "self"
	ActivePeer = "peer"
)

type State struct {
	FileSession    string `json:"file_session"`
	PeerFiles      bool   `json:"peer_files"`
	Connected      bool   `json:"connected"`
	Active         string `json:"active"`
	PeerName       string `json:"peer_name"`
	PeerOS         string `json:"peer_os"`
	PeerCanControl bool   `json:"peer_can_control"`
	SelfCanControl bool   `json:"self_can_control"`
}

// InputEvent 是一次采集到的键鼠事件，Typ 为 proto.MsgKey / MsgMouseMove / MsgMouseButton / MsgWheel。
type InputEvent struct {
	trace uint64
	Typ   byte
	A, B  int32
	C     byte
}

// Keys 保存热键及“录制新热键”的状态，由输入线程无锁读取。
type Keys struct {
	toggle    atomic.Uint64
	emergency atomic.Uint64
	suspend   atomic.Int64 // 在此时刻（UnixNano）之前处于录制状态
	captured  atomic.Uint64
}

// 输入线程对一次按键按下的处理结论
const (
	KeyPass      = iota // 不是热键，按正常流程处理
	KeySwallow          // 吞掉（正在录制，按键已交给面板）
	KeyToggle           // 切换热键
	KeyEmergency        // 紧急热键
)

func (k *Keys) Set(toggle, emergency Hotkey) {
	k.toggle.Store(toggle.Pack())
	k.emergency.Store(emergency.Pack())
}

// OnKeyDown 由输入线程在每次物理按键按下时调用。mods 为当前按住的修饰键，isModifier 表示这个键本身是修饰键。
func (k *Keys) OnKeyDown(vk, mods uint32, isModifier bool) int {
	cur := Hotkey{Mods: mods, VK: vk}.Pack()
	if time.Now().UnixNano() < k.suspend.Load() {
		if isModifier {
			return KeyPass
		}
		k.captured.Store(cur)
		return KeySwallow
	}
	switch cur {
	case k.emergency.Load():
		return KeyEmergency
	case k.toggle.Load():
		return KeyToggle
	}
	return KeyPass
}

// BeginCapture 开始（或续期）录制，几秒后自动失效，避免面板意外关闭后热键一直不可用。
func (k *Keys) BeginCapture() {
	if time.Now().UnixNano() >= k.suspend.Load() {
		k.captured.Store(0)
	}
	k.suspend.Store(time.Now().Add(8 * time.Second).UnixNano())
}

func (k *Keys) EndCapture() {
	k.suspend.Store(0)
	k.captured.Store(0)
}

// PollCapture 取走录制到的组合键。
func (k *Keys) PollCapture() (Hotkey, bool) {
	v := k.captured.Swap(0)
	return Unpack(v), v != 0
}

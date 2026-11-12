package core

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kvmswitch/internal/proto"
	"kvmswitch/internal/xfer"
)

// Options 是启动核心时由各平台决定的参数。
type Options struct {
	ConfigPath string
	// Listen 只决定“谁拨号、谁接听”这个网络细节，不代表主从：
	// 连接建立后两端的能力完全相同。
	Listen bool
}

type cutState struct {
	paths        []string
	done         bool // 文件已完整传到对端
	pastePending bool // 对端已粘贴，但传输确认还没回来
}

type ddcAttempt struct {
	seq                 uint64
	target, prev        string
	localDone, peerDone bool
	anyOK               bool
	errs                []string
}

// Core 是切换状态机。除标注外，字段只在 loop() 所在的 goroutine 里读写。
type Core struct {
	P    Platform
	Keys Keys
	opt  Options

	cfgp    atomic.Pointer[Config] // 配置视为不可变，修改时整体替换
	keyp    atomic.Pointer[[]byte]
	activeA atomic.Int32 // active 的原子镜像，供读连接的 goroutine 判断是否注入
	ev      chan func()
	input   chan InputEvent
	kick    chan struct{} // 让拨号循环立即重试

	parsed   Parsed
	active   string
	ctrl     *proto.Conn
	ctrlStop chan struct{}
	data     *xfer.Session
	peer     proto.Info
	selfCan  bool

	activity             atomic.Uint64
	offerID, offerChange uint64
	pending              atomic.Pointer[clipTicket]
	transfer             atomic.Pointer[Transfer]
	lastClip             uint64 // 最近一次已同步（或由我们写入）的剪贴板变化计数
	nextID               uint64
	cuts                 map[uint64]*cutState // 本机“剪切”后发往对端的文件
	pendCutID            uint64               // 对端“剪切”来的文件，等待本机粘贴
	pendCutClp           uint64

	seq uint64
	ddc *ddcAttempt

	probeBusy atomic.Bool
	stMu      sync.Mutex
	st        State
}

// New 读取配置并创建核心。created 表示本机是首次运行。
func New(p Platform, opt Options) (c *Core, created bool, err error) {
	cfg, created, err := LoadConfig(opt.ConfigPath)
	if err != nil {
		return nil, false, err
	}
	parsed, err := Validate(cfg)
	if err != nil {
		return nil, false, fmt.Errorf("配置有误：%v", err)
	}
	c = &Core{P: p, opt: opt, ev: make(chan func(), 256), input: make(chan InputEvent, 8192),
		kick: make(chan struct{}, 1), cuts: map[uint64]*cutState{}, active: ActiveNone}
	key := NormKey(cfg.PairingKey)
	c.keyp.Store(&key)
	c.install(cfg, parsed)
	return c, created, nil
}

func (c *Core) Config() *Config { return c.cfgp.Load() }

func (c *Core) State() State {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	return c.st
}

func (c *Core) do(f func()) { c.ev <- f }

// try 用于输入线程，绝不阻塞。
func (c *Core) try(f func()) {
	select {
	case c.ev <- f:
	default:
	}
}

// Start 启动事件循环与网络。
func (c *Core) Start() error {
	c.selfCan = c.P.CanControl()
	c.refresh()
	go func() {
		for f := range c.ev {
			f()
		}
	}()
	go c.watchPermission()
	if c.opt.Listen {
		return c.listen()
	}
	go c.dialLoop()
	return nil
}

func (c *Core) install(cfg *Config, p Parsed) {
	if !cfg.SyncClipboard {
		c.pending.Store(nil)
		c.offerID = 0
		c.transfer.Store(nil)
		c.pendCutID = 0
	}
	c.cfgp.Store(cfg)
	c.parsed = p
	c.Keys.Set(p.Toggle, p.Emergency)
	c.P.ApplyTuning(cfg)
}

// inputs 返回本机与对端在显示器上的输入口取值。
func (c *Core) inputs() (self, peer uint16) {
	if c.P.OS() == "windows" {
		return c.parsed.WinInput, c.parsed.MacInput
	}
	return c.parsed.MacInput, c.parsed.WinInput
}

func (c *Core) refresh() {
	s := State{
		Connected:      c.ctrl != nil,
		Active:         c.active,
		PeerName:       c.peer.Name,
		PeerOS:         c.peer.OS,
		PeerCanControl: c.peer.CanControl,
		SelfCanControl: c.selfCan,
	}
	c.stMu.Lock()
	c.st = s
	c.stMu.Unlock()
	c.P.OnState(s)
}

func (c *Core) selfInfo() proto.Info {
	return proto.Info{Name: c.P.Hostname(), OS: c.P.OS(), CanControl: c.selfCan, Version: Version}
}

func (c *Core) watchPermission() {
	last := c.P.CanControl()
	for range time.Tick(2 * time.Second) {
		if can := c.P.CanControl(); can != last {
			last = can
			c.do(func() {
				c.selfCan = can
				if c.ctrl != nil {
					c.ctrl.WriteJSON(proto.MsgInfo, c.selfInfo())
				}
				c.refresh()
			})
		}
	}
}

// ---------------------------------------------------------------- 对外操作

// Hotkey 由输入线程在识别到热键时调用，不会阻塞。
func (c *Core) Hotkey(emergency bool) {
	if emergency {
		c.try(c.takeHere)
	} else {
		c.try(c.toggle)
	}
}

// Toggle 供面板和托盘菜单调用。
func (c *Core) Toggle() { c.try(c.toggle) }

// PushInput 由输入线程在转发状态下调用；队列满时丢弃，绝不阻塞。
func (c *Core) PushInput(ev InputEvent) {
	select {
	case c.input <- ev:
	default:
	}
}

// LocalPaste 由输入线程在本机物理键盘按下粘贴组合键时调用。
func (c *Core) LocalPaste() { c.try(c.checkPaste) }

// ApplyConfig 校验、保存并即时启用面板提交的设置，然后同步给对端。
func (c *Core) ApplyConfig(next *Config) error {
	res := make(chan error, 1)
	c.do(func() {
		old := c.Config()
		next.PairingKey, next.Port, next.PeerHost = old.PairingKey, old.Port, old.PeerHost
		next.Rev = max(nowMs(), old.Rev+1)
		p, err := Validate(next)
		if err == nil {
			err = SaveConfig(c.opt.ConfigPath, next)
		}
		if err != nil {
			res <- err
			return
		}
		c.install(next, p)
		if c.ctrl != nil {
			c.ctrl.WriteJSON(proto.MsgConfig, next.Shared())
		}
		c.refresh()
		log.Printf("设置已更新")
		res <- nil
	})
	return <-res
}

// SetPairing 更换配对密钥和（可选的）对方地址，并用新密钥重新建立连接。
func (c *Core) SetPairing(key, host string) error {
	key = FormatKey(key)
	if key == "" {
		return errors.New("密钥格式不对，应为 20 位字母数字（XXXXX-XXXXX-XXXXX-XXXXX）")
	}
	host = strings.TrimSpace(host)
	res := make(chan error, 1)
	c.do(func() {
		next := *c.Config()
		next.PairingKey, next.PeerHost = key, host
		if err := SaveConfig(c.opt.ConfigPath, &next); err != nil {
			res <- err
			return
		}
		c.cfgp.Store(&next)
		k := NormKey(key)
		c.keyp.Store(&k)
		if c.ctrl != nil {
			c.ctrl.Close()
		}
		if c.data != nil {
			c.data.C.Close()
		}
		select {
		case c.kick <- struct{}{}:
		default:
		}
		log.Printf("配对信息已更新，正在重新连接")
		res <- nil
	})
	return <-res
}

// MonitorTest 把显示器切到对端的输入口，10 秒后尝试切回。backErr 为切回时的错误（常见且不致命）。
func (c *Core) MonitorTest() (backErr string, err error) {
	if !c.probeBusy.CompareAndSwap(false, true) {
		return "", errors.New("已有输入口测试进行中")
	}
	defer c.probeBusy.Store(false)
	type in struct {
		self, peer uint16
		on, busy   bool
	}
	ch := make(chan in, 1)
	c.do(func() {
		s, p := c.inputs()
		ch <- in{s, p, c.Config().SwitchMonitor, c.active == ActivePeer}
	})
	v := <-ch
	if !v.on {
		return "", errors.New("请先开启“切换时同时切换显示器”")
	}
	if v.busy {
		return "", errors.New("键鼠当前不在本机，无法测试")
	}
	cfg := c.Config()
	if err := c.P.SetMonitorInput(v.peer, cfg); err != nil {
		return "", fmt.Errorf("切换指令发送失败：%v", err)
	}
	log.Printf("显示器测试：已切到对端输入口，10 秒后尝试切回")
	time.Sleep(10 * time.Second)
	if err := c.P.SetMonitorInput(v.self, cfg); err != nil {
		return err.Error(), nil
	}
	return "", nil
}

// ---------------------------------------------------------------- 切换

func (c *Core) peerLabel() string {
	if c.peer.Name != "" {
		return c.peer.Name
	}
	return "另一台设备"
}

func (c *Core) toggle() {
	if c.active == ActivePeer {
		c.switchTo(ActiveSelf)
	} else {
		c.switchTo(ActivePeer)
	}
}

// takeHere 是紧急热键：无论当前状态如何，把键鼠和显示器都拿回按下热键的这台设备。
func (c *Core) takeHere() {
	if c.ctrl == nil {
		c.setActive(ActiveNone)
		if c.Config().SwitchMonitor {
			self, _ := c.inputs()
			cfg := c.Config()
			go func() {
				if err := c.P.SetMonitorInput(self, cfg); err != nil {
					log.Printf("切回显示器未成功: %v", err)
				}
			}()
		}
		return
	}
	c.switchTo(ActiveSelf)
}

func (c *Core) switchTo(target string) {
	if c.ctrl == nil {
		c.P.Beep()
		c.P.Notify("无法切换", "另一台设备未连接。请确认它上面的 KVMSwitch 正在运行，且两台设备在同一局域网。")
		return
	}
	if target == ActivePeer && !c.peer.CanControl {
		c.P.Beep()
		c.P.Notify("无法切换", c.peerLabel()+" 还没有授予“辅助功能”权限，切过去后键鼠无法使用。")
		return
	}
	if target == ActiveSelf && !c.selfCan {
		c.P.Beep()
		c.P.Notify("无法切换", "本机还没有授予“辅助功能”权限，无法接受另一台设备的键鼠。")
		return
	}
	prev := c.active
	c.seq++
	mon := c.Config().SwitchMonitor
	who := "sender"
	if target == ActivePeer {
		who = "receiver"
	}
	if err := c.ctrl.WriteJSON(proto.MsgActive, proto.Active{Who: who, Seq: c.seq, Monitor: mon}); err != nil {
		log.Printf("发送切换指令失败: %v", err)
		return
	}
	c.setActive(target)
	if mon {
		c.startDDC(target, prev)
	}
}

// setActive 落实键鼠归属变化在本机的全部效果。
func (c *Core) setActive(a string) {
	old := c.active
	if old != a {
		c.pending.Store(nil)
		c.offerID = 0
		c.transfer.Store(nil)
	}
	c.active = a
	switch a {
	case ActiveSelf:
		c.activeA.Store(1)
	case ActivePeer:
		c.activeA.Store(2)
	default:
		c.activeA.Store(0)
	}
	if a == ActivePeer && old != ActivePeer && c.Config().SyncClipboard {
		c.pushClipboard() // 键鼠要离开本机了：把本机新复制的内容带过去
	}
	if a != ActivePeer {
		c.P.SetForward(false)
		c.drainInput()
	}
	c.P.SetControlled(a == ActiveSelf)
	if a == ActivePeer {
		c.drainInput()
		c.P.SetForward(true)
	}
	c.P.KeepAwake(a != ActiveNone)
	c.refresh()
	if a != old {
		switch a {
		case ActiveSelf:
			log.Printf("键鼠切到本机")
		case ActivePeer:
			log.Printf("键鼠切到 %s", c.peerLabel())
		default:
			log.Printf("两台设备恢复各自独立")
		}
	}
}

func (c *Core) drainInput() {
	for {
		select {
		case <-c.input:
		default:
			return
		}
	}
}

// onActive 处理对端发起的切换。
func (c *Core) onActive(m proto.Active) {
	target := ActiveNone
	switch m.Who {
	case "sender":
		target = ActivePeer
	case "receiver":
		target = ActiveSelf
	}
	c.ddc = nil // 对端发起了新的切换，本机之前发起的那次不再需要裁决
	c.setActive(target)
	if !m.Monitor || target == ActiveNone {
		return
	}
	// 两端都尝试让显示器换口：通常只有当前正在显示的那一端发出的指令会被显示器接受
	self, peer := c.inputs()
	v := self
	if target == ActivePeer {
		v = peer
	}
	cfg, pc := c.Config(), c.ctrl
	go func() {
		r := proto.DDCResult{Seq: m.Seq, OK: true}
		if err := c.P.SetMonitorInput(v, cfg); err != nil {
			r.OK, r.Err = false, err.Error()
		}
		if pc != nil {
			pc.WriteJSON(proto.MsgDDCResult, r)
		}
	}()
}

func (c *Core) startDDC(target, prev string) {
	att := &ddcAttempt{seq: c.seq, target: target, prev: prev}
	c.ddc = att
	self, peer := c.inputs()
	v := self
	if target == ActivePeer {
		v = peer
	}
	cfg := c.Config()
	go func() {
		err := c.P.SetMonitorInput(v, cfg)
		c.do(func() { c.ddcReport(att.seq, true, "本机", err) })
	}()
	time.AfterFunc(3*time.Second, func() {
		c.do(func() { c.ddcReport(att.seq, false, "对端", errors.New("没有回应")) })
	})
}

// ddcReport 汇总两端的显示器切换结果：只要有一端成功就算成功；都失败则撤销这次切换。
func (c *Core) ddcReport(seq uint64, local bool, who string, err error) {
	att := c.ddc
	if att == nil || att.seq != seq {
		return
	}
	if local {
		if att.localDone {
			return
		}
		att.localDone = true
	} else {
		if att.peerDone {
			return
		}
		att.peerDone = true
	}
	if err == nil {
		att.anyOK = true
	} else {
		att.errs = append(att.errs, who+"："+err.Error())
	}
	if att.anyOK {
		c.ddc = nil
		return
	}
	if !att.localDone || !att.peerDone {
		return
	}
	c.ddc = nil
	msg := strings.Join(att.errs, "；")
	log.Printf("显示器切换失败: %s", msg)
	if c.active == att.target && c.ctrl != nil {
		who := "none"
		switch att.prev {
		case ActiveSelf:
			who = "sender"
		case ActivePeer:
			who = "receiver"
		}
		c.seq++
		c.ctrl.WriteJSON(proto.MsgActive, proto.Active{Who: who, Seq: c.seq})
		c.setActive(att.prev)
	}
	c.P.Notify("显示器切换失败，已撤销本次切换", msg)
}

// ---------------------------------------------------------------- 剪贴板

// pushClipboard 若本机剪贴板有新内容，则发给对端。
func (c *Core) pushClipboard() {
	chg := c.P.ClipChange()
	if chg == 0 || chg == c.lastClip || c.ctrl == nil {
		return
	}
	if formats, ok := c.P.(interface{ HasClip() bool }); ok && !formats.HasClip() {
		return
	}
	c.nextID++
	c.offerID, c.offerChange = c.nextID, chg
	c.sendClipID(proto.MsgClipOffer, c.offerID)
}

func (c *Core) sendClipID(typ byte, id uint64) {
	if c.ctrl == nil {
		return
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	c.ctrl.Write(typ, b[:])
}

type pasteHost interface {
	PasteApp() uintptr
	PasteFocus() uintptr
	PasteNow(app, focus uintptr) bool
}

func (c *Core) Interacted() { c.activity.Add(1) }

type clipTicket struct {
	app, focus uintptr
	epoch      uint64
	id, local  uint64
	started    atomic.Bool
}
type Transfer struct {
	Status      string
	Done, Total int64
}

func (c *Core) TransferState() *Transfer { return c.transfer.Load() }

// Called before the OS sees a paste shortcut. Never wait for network I/O in a keyboard callback.
func (c *Core) RequestPaste() bool {
	t := c.pending.Load()
	if t == nil || !c.Config().SyncClipboard || c.activeA.Load() != 1 {
		return false
	}
	if t.local != c.P.ClipChange() {
		if c.pending.CompareAndSwap(t, nil) {
			c.transfer.Store(&Transfer{Status: "本机已复制新内容，使用本机剪贴板"})
		}
		return false
	}
	if t.started.CompareAndSwap(false, true) {
		epoch := c.activity.Load()
		var app uintptr
		host, native := c.P.(pasteHost)
		if native {
			app = host.PasteApp()
		}
		select {
		case c.ev <- func() {
			if c.pending.Load() == t {
				t.app, t.epoch = app, epoch
				if native && app != 0 && host.PasteApp() == app && c.activity.Load() == epoch {
					t.focus = host.PasteFocus()
				}
				c.transfer.Store(&Transfer{Status: "正在准备传输"})
				c.sendClipID(proto.MsgClipFetch, t.id)
			}
		}:
		default:
			t.started.Store(false)
		}
	}
	return true
}

func (c *Core) fetchClip(id uint64) {
	if id != c.offerID || c.active != ActivePeer || !c.Config().SyncClipboard || c.data == nil || c.P.ClipChange() != c.offerChange {
		c.sendClipID(proto.MsgClipUnavailable, id)
		return
	}
	clip, ok := c.P.ReadClip()
	if !ok || c.P.ClipChange() != c.offerChange {
		c.sendClipID(proto.MsgClipUnavailable, id)
		return
	}
	c.offerID = 0
	change := c.offerChange
	sess := c.data
	c.cuts = map[uint64]*cutState{}
	if clip.Kind == xfer.KindFiles && clip.Cut {
		c.cuts[id] = &cutState{paths: clip.Paths}
	}
	go func() {
		err := sess.Send(id, clip)
		if err != nil {
			log.Printf("剪贴板发送失败: %v", err)
		}
		c.do(func() {
			if c.data != sess {
				return
			}
			st := c.cuts[id]
			if err != nil {
				delete(c.cuts, id)
				c.sendClipID(proto.MsgClipUnavailable, id)
				if clip.Kind == xfer.KindFiles {
					c.lastClip = 0 // 下次切换时重试
				}
				return
			}
			c.lastClip = change
			if st != nil {
				st.done = true
				if st.pastePending {
					c.finishCut(id)
				}
			}
		})
	}()
}

func (c *Core) finishCut(id uint64) {
	st := c.cuts[id]
	if st == nil {
		return
	}
	delete(c.cuts, id)
	paths := st.paths
	go func() {
		if err := c.P.Recycle(paths); err != nil {
			log.Printf("剪切完成但源文件移入回收站失败: %v", err)
		} else {
			log.Printf("剪切完成，%d 个源文件已移入回收站", len(paths))
		}
	}()
}

// onPaste：对端报告它粘贴了我们“剪切”过去的文件。
func (c *Core) onPaste(id uint64) {
	st := c.cuts[id]
	if st == nil {
		return
	}
	if st.done {
		c.finishCut(id)
	} else {
		st.pastePending = true
	}
}

// checkPaste：本机发生了一次粘贴按键。如果剪贴板里仍是对端“剪切”来的文件，通知对端移走源文件。
func (c *Core) checkPaste() {
	if c.pendCutID == 0 {
		return
	}
	id := c.pendCutID
	c.pendCutID = 0
	if c.P.ClipChange() != c.pendCutClp || c.ctrl == nil {
		return
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, id)
	c.ctrl.Write(proto.MsgPasteDetected, b)
	log.Printf("检测到粘贴，通知对端完成剪切")
}

// ---------------------------------------------------------------- 连接

func (c *Core) listen() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Config().Port))
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			go func() {
				pc, err := proto.Server(conn, *c.keyp.Load())
				if err != nil {
					log.Printf("拒绝来自 %v 的连接: %v", conn.RemoteAddr(), err)
					conn.Close()
					return
				}
				switch pc.Channel {
				case proto.ChanControl:
					c.runControl(pc)
				case proto.ChanData:
					c.runData(pc)
				default:
					pc.Close()
				}
			}()
		}
	}()
	go c.discoveryResponder()
	return nil
}

// discoveryResponder 响应局域网广播，让对端能自动找到本机。
func (c *Core) discoveryResponder() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: proto.UDPPort})
	if err != nil {
		log.Printf("自动发现不可用（UDP %d 被占用）: %v", proto.UDPPort, err)
		return
	}
	buf := make([]byte, 256)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if strings.TrimSpace(string(buf[:n])) == proto.DiscoverRequest {
			conn.WriteToUDP([]byte(fmt.Sprintf("%s %d", proto.DiscoverReply, c.Config().Port)), addr)
		}
	}
}

// discover 通过局域网广播寻找对端。
func discover() (string, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	targets := []net.IP{net.IPv4bcast}
	if ifs, err := net.Interfaces(); err == nil {
		for _, ifc := range ifs {
			if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					if ip4 := n.IP.To4(); ip4 != nil && len(n.Mask) == 4 {
						b := make(net.IP, 4)
						for i := range b {
							b[i] = ip4[i] | ^n.Mask[i]
						}
						targets = append(targets, b)
					}
				}
			}
		}
	}
	for _, t := range targets {
		conn.WriteToUDP([]byte(proto.DiscoverRequest), &net.UDPAddr{IP: t, Port: proto.UDPPort})
	}
	conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 256)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", errors.New("局域网内没有发现另一台设备")
		}
		f := strings.Fields(string(buf[:n]))
		if len(f) == 2 && f[0] == proto.DiscoverReply {
			if _, err := strconv.Atoi(f[1]); err == nil {
				return net.JoinHostPort(addr.IP.String(), f[1]), nil
			}
		}
	}
}

func (c *Core) dialLoop() {
	last := ""
	lastErr := ""
	for {
		err := c.dialOnce(&last)
		if err != nil && err.Error() != lastErr {
			lastErr = err.Error()
			log.Printf("尚未连接: %v", err)
		} else if err == nil {
			lastErr = ""
		}
		select {
		case <-c.kick:
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Core) dialOnce(last *string) error {
	cfg := c.Config()
	var cands []string
	if cfg.PeerHost != "" {
		h := cfg.PeerHost
		if _, _, e := net.SplitHostPort(h); e != nil {
			h = net.JoinHostPort(h, strconv.Itoa(proto.TCPPort))
		}
		cands = append(cands, h)
	} else {
		if *last != "" {
			cands = append(cands, *last)
		}
		if a, e := discover(); e == nil && a != *last {
			cands = append(cands, a)
		}
	}
	err := errors.New("局域网内没有发现另一台设备")
	key := *c.keyp.Load()
	dial := func(addr string, ch byte) (*proto.Conn, error) {
		conn, e := net.DialTimeout("tcp", addr, 3*time.Second)
		if e != nil {
			return nil, e
		}
		pc, e := proto.Client(conn, key, ch)
		if e != nil {
			conn.Close()
		}
		return pc, e
	}
	for _, a := range cands {
		var ctrl, data *proto.Conn
		if ctrl, err = dial(a, proto.ChanControl); err != nil {
			continue
		}
		if data, err = dial(a, proto.ChanData); err != nil {
			ctrl.Close()
			continue
		}
		*last = a
		go func() {
			c.runData(data)
			ctrl.Close() // 任一通道断开都重建整个会话
		}()
		c.runControl(ctrl)
		data.Close()
		return nil
	}
	return err
}

// runControl 接管一条控制通道，阻塞到连接断开。
func (c *Core) runControl(pc *proto.Conn) {
	stop := make(chan struct{})
	c.do(func() {
		if c.ctrl != nil {
			old := c.ctrl
			c.detach()
			old.Close()
		}
		c.ctrl, c.ctrlStop = pc, stop
		c.peer = proto.Info{}
		pc.WriteJSON(proto.MsgInfo, c.selfInfo())
		pc.WriteJSON(proto.MsgConfig, c.Config().Shared())
		go c.pump(pc, stop)
		c.refresh()
		log.Printf("已连接: %v", pc.RemoteAddr())
	})
	for {
		pc.SetReadDeadline(time.Now().Add(8 * time.Second))
		t, p, err := pc.Read()
		if err != nil {
			break
		}
		switch t {
		case proto.MsgInfo:
			var in proto.Info
			if json.Unmarshal(p, &in) == nil {
				c.do(func() {
					if c.ctrl == pc {
						c.peer = in
						c.refresh()
					}
				})
			}
		case proto.MsgConfig:
			var s Shared
			if json.Unmarshal(p, &s) == nil {
				c.do(func() { c.onPeerConfig(s) })
			}
		case proto.MsgActive:
			var m proto.Active
			if json.Unmarshal(p, &m) == nil {
				c.do(func() {
					if c.ctrl == pc {
						c.onActive(m)
					}
				})
			}
		case proto.MsgDDCResult:
			var r proto.DDCResult
			if json.Unmarshal(p, &r) == nil {
				c.do(func() {
					var err error
					if !r.OK {
						err = errors.New(r.Err)
					}
					c.ddcReport(r.Seq, false, c.peerLabel(), err)
				})
			}
		case proto.MsgClipOffer, proto.MsgClipFetch, proto.MsgClipUnavailable:
			if len(p) != 8 {
				continue
			}
			id := binary.BigEndian.Uint64(p)
			c.do(func() {
				if c.ctrl != pc {
					return
				}
				switch t {
				case proto.MsgClipFetch:
					c.fetchClip(id)
				case proto.MsgClipOffer:
					if c.active == ActiveSelf && c.Config().SyncClipboard {
						c.pending.Store(&clipTicket{id: id, local: c.P.ClipChange()})
						c.transfer.Store(&Transfer{Status: "等待粘贴"})
					}
				case proto.MsgClipUnavailable:
					if q := c.pending.Load(); q != nil && q.id == id {
						c.pending.Store(nil)
						c.transfer.Store(&Transfer{Status: "未传输：内容已变更、为空或传输已取消"})
					}
				}
			})
		case proto.MsgPasteDetected:
			if len(p) >= 8 {
				id := binary.BigEndian.Uint64(p)
				c.do(func() { c.onPaste(id) })
			}
		case proto.MsgKey:
			if len(p) >= 5 && c.activeA.Load() == 1 {
				if p[4]&proto.KeyFlagDown != 0 {
					c.Interacted()
				}
				if c.P.InjectKey(binary.BigEndian.Uint16(p), p[4]) {
					c.try(c.checkPaste)
				}
			}
		case proto.MsgMouseMove:
			if len(p) >= 8 && c.activeA.Load() == 1 {
				c.P.InjectMove(int32(binary.BigEndian.Uint32(p)), int32(binary.BigEndian.Uint32(p[4:])))
			}
		case proto.MsgMouseButton:
			if len(p) >= 2 && c.activeA.Load() == 1 {
				c.Interacted()
				c.P.InjectButton(int(p[0]), p[1] != 0)
			}
		case proto.MsgWheel:
			if len(p) >= 8 && c.activeA.Load() == 1 {
				c.Interacted()
				c.P.InjectWheel(int32(binary.BigEndian.Uint32(p)), int32(binary.BigEndian.Uint32(p[4:])))
			}
		}
	}
	pc.Close()
	c.do(func() {
		if c.ctrl == pc {
			c.detach()
		}
	})
}

// detach 处理控制通道断开：两台设备恢复各自独立。
func (c *Core) detach() {
	c.cuts = map[uint64]*cutState{}
	c.pendCutID = 0
	c.lastClip = 0
	c.pending.Store(nil)
	c.offerID = 0
	close(c.ctrlStop)
	c.ctrl, c.ctrlStop = nil, nil
	c.peer = proto.Info{}
	c.ddc = nil
	was := c.active
	log.Printf("连接已断开")
	c.setActive(ActiveNone)
	if was == ActivePeer {
		// 键鼠在本机而画面在对端：尽力把显示器拿回来
		c.P.Notify("已切回本机", "与另一台设备的连接中断。")
		if cfg := c.Config(); cfg.SwitchMonitor {
			self, _ := c.inputs()
			go func() {
				if err := c.P.SetMonitorInput(self, cfg); err != nil {
					log.Printf("切回显示器未成功: %v", err)
				}
			}()
		}
	}
}

func (c *Core) onPeerConfig(s Shared) {
	cur := c.Config()
	if s.Rev <= cur.Rev {
		return
	}
	next := cur.WithShared(s)
	p, err := Validate(next)
	if err != nil {
		log.Printf("忽略对端发来的设置: %v", err)
		return
	}
	if err := SaveConfig(c.opt.ConfigPath, next); err != nil {
		log.Printf("保存对端同步来的设置失败: %v", err)
	}
	c.install(next, p)
	c.refresh()
	log.Printf("已同步对端的设置")
}

// pump 把采集到的输入事件写到控制通道，合并连续的鼠标移动，并定时发送心跳。
func (c *Core) pump(pc *proto.Conn, stop chan struct{}) {
	ping := time.NewTicker(2 * time.Second)
	defer ping.Stop()
	var pending *InputEvent
	buf := make([]byte, 8)
	for {
		var ev InputEvent
		if pending != nil {
			ev, pending = *pending, nil
		} else {
			select {
			case ev = <-c.input:
			case <-ping.C:
				if pc.Write(proto.MsgPing, nil) != nil {
					pc.Close()
					return
				}
				continue
			case <-stop:
				return
			}
		}
		if ev.Typ == proto.MsgMouseMove {
		merge:
			for {
				select {
				case n := <-c.input:
					if n.Typ == proto.MsgMouseMove {
						ev.A += n.A
						ev.B += n.B
					} else {
						pending = &n
						break merge
					}
				default:
					break merge
				}
			}
		}
		var out []byte
		switch ev.Typ {
		case proto.MsgKey:
			binary.BigEndian.PutUint16(buf[0:], uint16(ev.A))
			binary.BigEndian.PutUint16(buf[2:], uint16(ev.B))
			buf[4] = ev.C
			out = buf[:5]
		case proto.MsgMouseMove, proto.MsgWheel:
			binary.BigEndian.PutUint32(buf[0:], uint32(ev.A))
			binary.BigEndian.PutUint32(buf[4:], uint32(ev.B))
			out = buf[:8]
		case proto.MsgMouseButton:
			buf[0], buf[1] = byte(ev.A), ev.C
			out = buf[:2]
		default:
			continue
		}
		if pc.Write(ev.Typ, out) != nil {
			pc.Close()
			return
		}
	}
}

// runData 接管一条数据通道（剪贴板与文件），阻塞到连接断开。
func (c *Core) runData(pc *proto.Conn) {
	s := &xfer.Session{
		C:       pc,
		Dir:     c.P.RecvDir(),
		MaxAuto: func() int64 { return c.Config().MaxAutoFileMB << 20 },
		Confirm: func(h xfer.Header) bool {
			return c.P.Confirm(fmt.Sprintf("另一台设备要传来 %d 个项目，共 %s，超过了自动同步上限（%d MB）。\n\n是否接收？",
				len(h.Files), xfer.HumanSize(h.Total), c.Config().MaxAutoFileMB))
		},
		Logf: log.Printf,
	}
	s.Wanted = func(id uint64) bool {
		q := c.pending.Load()
		return q != nil && q.id == id && q.started.Load() && c.Config().SyncClipboard
	}
	s.Progress = func(id uint64, done, total int64) {
		if q := c.pending.Load(); q != nil && q.id == id {
			c.transfer.Store(&Transfer{Status: "接收中", Done: done, Total: total})
		}
	}
	s.Failed = func(id uint64) {
		c.do(func() {
			if q := c.pending.Load(); q != nil && q.id == id {
				c.pending.Store(nil)
				c.transfer.Store(&Transfer{Status: "接收失败或已取消"})
			}
		})
	}
	s.OnClip = func(h xfer.Header, data []byte, paths []string) {
		c.do(func() {
			q := c.pending.Load()
			if c.data != s || q == nil || !q.started.Load() || q.id != h.ID {
				return
			}
			defer c.pending.CompareAndSwap(q, nil)
			if !c.Config().SyncClipboard || c.P.ClipChange() != q.local {
				c.transfer.Store(&Transfer{Status: "本机已复制新内容，未覆盖剪贴板"})
				return
			}
			chg, err := c.P.WriteClip(h.Kind, data, paths)
			if err != nil {
				c.transfer.Store(&Transfer{Status: "写入剪贴板失败"})
				log.Printf("写入剪贴板失败: %v", err)
				return
			}
			log.Printf("收到剪贴板: %s，%s", h.Kind, xfer.HumanSize(h.Total))
			auto := false
			if host, ok := c.P.(pasteHost); ok && !h.Cut && q.app != 0 && q.focus != 0 && q.epoch == c.activity.Load() && c.P.ClipChange() == chg && c.active == ActiveSelf {
				auto = host.PasteNow(q.app, q.focus)
			}
			if auto {
				c.transfer.Store(&Transfer{Status: "已发送粘贴快捷键", Done: h.Total, Total: h.Total})
			} else {
				c.transfer.Store(&Transfer{Status: "已就绪，请在目标位置粘贴", Done: h.Total, Total: h.Total})
				c.P.Notify("剪贴板已就绪", "请在目标位置再次粘贴。")
			}
			c.lastClip = chg
			c.pendCutID, c.pendCutClp = 0, 0
			if h.Kind == xfer.KindFiles && h.Cut {
				c.pendCutID, c.pendCutClp = h.ID, chg
			}
		})
	}
	c.do(func() {
		if c.data != nil {
			c.data.C.Close()
		}
		c.data = s
	})
	s.Run()
	pc.Close()
	c.do(func() {
		if c.data == s {
			c.pending.Store(nil)
			c.transfer.Store(&Transfer{Status: "数据通道断开"})
			c.data = nil
		}
	})
}

// ProbePort never persists guesses; restoration does not depend on the panel staying open.
func (c *Core) ProbePort(name string) error {
	v, ok := map[string]uint16{"hdmi1": 17, "hdmi2": 18, "dp1": 15, "dp2": 16}[name]
	if !ok {
		return errors.New("未知的候选接口")
	}
	if !c.probeBusy.CompareAndSwap(false, true) {
		return errors.New("正在测试其他接口")
	}
	defer c.probeBusy.Store(false)
	if c.State().Active == ActivePeer {
		return errors.New("请先把键鼠切回本机")
	}
	cfg := c.Config()
	back := cfg.WindowsInput
	if c.P.OS() == "mac" {
		back = cfg.MacInput
	}
	origin, err := ParseInput(back)
	if err != nil {
		return err
	}
	if reader, ok := c.P.(interface{ CurrentInput() (uint16, error) }); ok {
		origin, err = reader.CurrentInput()
		if err != nil {
			return err
		}
	}
	if err = c.P.SetMonitorInput(v, cfg); err != nil {
		return err
	}
	time.Sleep(6 * time.Second)
	if err = c.P.SetMonitorInput(origin, cfg); err != nil {
		return fmt.Errorf("已试切，但未能恢复，请用显示器按键切回：%w", err)
	}
	return nil
}

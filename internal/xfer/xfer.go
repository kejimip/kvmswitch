// Package xfer 在数据通道上传输剪贴板内容（文本、图片、文件/文件夹）。
package xfer

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kvmswitch/internal/proto"
)

const (
	KindText  = "text"
	KindImage = "image"
	KindFiles = "files"

	maxMemClip   = 512 << 20
	maxHeaderLen = 3 << 20
)

type FileEntry struct {
	Path string `json:"p"` // 相对路径，使用 /
	Dir  bool   `json:"d,omitempty"`
	Size int64  `json:"s,omitempty"`
}

type Header struct {
	ID      uint64      `json:"id"`
	Kind    string      `json:"kind"`
	Cut     bool        `json:"cut,omitempty"`
	Confirm bool        `json:"confirm,omitempty"`
	Total   int64       `json:"total"`
	Files   []FileEntry `json:"files,omitempty"`
}

// Clip 是待发送的本地剪贴板内容。
type Clip struct {
	Kind  string
	Data  []byte   // 文本(UTF-8) 或 图片(PNG)
	Paths []string // 文件：顶层绝对路径
	Cut   bool
}

// Session 封装一条数据通道连接，负责双向收发。
type Session struct {
	C        *proto.Conn
	Dir      string                                         // 接收文件的缓存根目录
	MaxAuto  func() int64                                   // 超过该字节数的文件需要对端确认；<=0 表示不限
	Confirm  func(h Header) bool                            // 收到需确认的传输时询问用户
	OnClip   func(h Header, data []byte, topPaths []string) // 一次接收完成
	Logf     func(format string, a ...any)
	sendMu   sync.Mutex
	replies  chan bool
	initOnce sync.Once

	// 接收状态
	rh      *Header
	rbuf    []byte
	rdir    string
	ridx    int
	rfile   *os.File
	rremain int64
	rgot    int64
}

func (s *Session) init() {
	s.initOnce.Do(func() { s.replies = make(chan bool, 1) })
}

func (s *Session) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

type srcFile struct {
	abs  string
	size int64
}

// buildList 展开顶层路径，生成条目列表。
func buildList(paths []string) (entries []FileEntry, srcs []srcFile, total int64, err error) {
	seen := map[string]bool{}
	for _, top := range paths {
		top = filepath.Clean(top)
		name := filepath.Base(top)
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		st, e := os.Lstat(top)
		if e != nil {
			return nil, nil, 0, e
		}
		if st.Mode().IsRegular() {
			entries = append(entries, FileEntry{Path: name, Size: st.Size()})
			srcs = append(srcs, srcFile{top, st.Size()})
			total += st.Size()
			continue
		}
		if !st.IsDir() {
			continue
		}
		e = filepath.WalkDir(top, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return nil // 跳过无权限的条目
			}
			rel, _ := filepath.Rel(top, p)
			wire := name
			if rel != "." {
				wire = name + "/" + filepath.ToSlash(rel)
			}
			if d.IsDir() {
				entries = append(entries, FileEntry{Path: wire, Dir: true})
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			entries = append(entries, FileEntry{Path: wire, Size: info.Size()})
			srcs = append(srcs, srcFile{p, info.Size()})
			total += info.Size()
			return nil
		})
		if e != nil {
			return nil, nil, 0, e
		}
	}
	return
}

// Send 发送一份剪贴板内容；阻塞直到完成。
func (s *Session) Send(id uint64, clip Clip) error {
	s.init()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	h := Header{ID: id, Kind: clip.Kind, Cut: clip.Cut}
	var srcs []srcFile
	if clip.Kind == KindFiles {
		var err error
		h.Files, srcs, h.Total, err = buildList(clip.Paths)
		if err != nil {
			return err
		}
		if len(h.Files) == 0 {
			return errors.New("没有可传输的文件")
		}
		if s.MaxAuto != nil {
			if m := s.MaxAuto(); m > 0 && h.Total > m {
				h.Confirm = true
			}
		}
	} else {
		h.Total = int64(len(clip.Data))
		if h.Total > maxMemClip {
			return errors.New("剪贴板内容过大")
		}
	}
	hb, _ := json.Marshal(h)
	if len(hb) > maxHeaderLen {
		return fmt.Errorf("文件数量过多（%d 项），未同步", len(h.Files))
	}
	select {
	case <-s.replies:
	default:
	}
	if err := s.C.Write(proto.MsgClipBegin, hb); err != nil {
		return err
	}
	if h.Confirm {
		select {
		case ok := <-s.replies:
			if !ok {
				return errors.New("对端取消了大文件传输")
			}
		case <-time.After(90 * time.Second):
			s.C.Write(proto.MsgClipAbort, nil)
			return errors.New("等待对端确认超时")
		}
	}
	if clip.Kind != KindFiles {
		for off := 0; off < len(clip.Data); off += proto.ChunkSize {
			end := min(off+proto.ChunkSize, len(clip.Data))
			if err := s.C.Write(proto.MsgClipChunk, clip.Data[off:end]); err != nil {
				return err
			}
		}
		return s.C.Write(proto.MsgClipEnd, nil)
	}
	buf := make([]byte, proto.ChunkSize)
	for _, sf := range srcs {
		if err := s.sendFile(sf, buf); err != nil {
			s.C.Write(proto.MsgClipAbort, nil)
			return err
		}
	}
	return s.C.Write(proto.MsgClipEnd, nil)
}

func (s *Session) sendFile(sf srcFile, buf []byte) error {
	if sf.size == 0 {
		return nil
	}
	f, err := os.Open(sf.abs)
	if err != nil {
		return err
	}
	defer f.Close()
	remain := sf.size
	for remain > 0 {
		n := int64(len(buf))
		if remain < n {
			n = remain
		}
		if _, err := io.ReadFull(f, buf[:n]); err != nil {
			return fmt.Errorf("读取 %s 失败（文件在传输中被修改？）: %w", sf.abs, err)
		}
		if err := s.C.Write(proto.MsgClipChunk, buf[:n]); err != nil {
			return err
		}
		remain -= n
	}
	return nil
}

// Run 是读循环，连接断开时返回。
func (s *Session) Run() error {
	s.init()
	defer s.abortRecv()
	for {
		t, p, err := s.C.Read()
		if err != nil {
			return err
		}
		switch t {
		case proto.MsgClipAccept, proto.MsgClipReject:
			select {
			case s.replies <- t == proto.MsgClipAccept:
			default:
			}
		case proto.MsgClipBegin:
			s.abortRecv()
			var h Header
			if json.Unmarshal(p, &h) != nil {
				continue
			}
			if err := s.begin(&h); err != nil {
				s.logf("接收剪贴板失败: %v", err)
				s.abortRecv()
			}
		case proto.MsgClipChunk:
			if s.rh == nil {
				continue
			}
			if err := s.chunk(p); err != nil {
				s.logf("接收剪贴板失败: %v", err)
				s.abortRecv()
			}
		case proto.MsgClipEnd:
			if s.rh == nil {
				continue
			}
			s.finish()
		case proto.MsgClipAbort:
			s.abortRecv()
		}
	}
}

func (s *Session) begin(h *Header) error {
	if h.Confirm {
		ok := s.Confirm != nil && s.Confirm(*h)
		id := make([]byte, 8)
		binary.BigEndian.PutUint64(id, h.ID)
		if !ok {
			s.C.Write(proto.MsgClipReject, id)
			return nil
		}
		if err := s.C.Write(proto.MsgClipAccept, id); err != nil {
			return err
		}
	}
	s.rgot = 0
	if h.Kind != KindFiles {
		if h.Total < 0 || h.Total > maxMemClip {
			return errors.New("内容过大")
		}
		s.rbuf = make([]byte, 0, h.Total)
		s.rh = h
		return nil
	}
	dir := filepath.Join(s.Dir, strconv.FormatInt(time.Now().UnixMilli(), 10)+"-"+strconv.FormatUint(h.ID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.rdir = dir
	for i := range h.Files {
		rel, err := sanitize(h.Files[i].Path)
		if err != nil {
			return err
		}
		h.Files[i].Path = rel
		if h.Files[i].Dir {
			if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
				return err
			}
		}
	}
	s.rh = h
	s.ridx = -1
	s.rremain = 0
	return s.advance()
}

// advance 关闭已写完的文件并打开下一个；零长度文件直接创建。
func (s *Session) advance() error {
	for s.rremain == 0 {
		if s.rfile != nil {
			s.rfile.Close()
			s.rfile = nil
		}
		s.ridx++
		for s.ridx < len(s.rh.Files) && s.rh.Files[s.ridx].Dir {
			s.ridx++
		}
		if s.ridx >= len(s.rh.Files) {
			return nil
		}
		e := s.rh.Files[s.ridx]
		if e.Size < 0 {
			return errors.New("非法文件大小")
		}
		p := filepath.Join(s.rdir, e.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		s.rfile = f
		s.rremain = e.Size
	}
	return nil
}

func (s *Session) chunk(p []byte) error {
	s.rgot += int64(len(p))
	if s.rgot > s.rh.Total {
		return errors.New("收到的数据超出声明大小")
	}
	if s.rh.Kind != KindFiles {
		s.rbuf = append(s.rbuf, p...)
		return nil
	}
	for len(p) > 0 {
		if s.rfile == nil {
			return errors.New("数据多于文件列表")
		}
		n := int64(len(p))
		if n > s.rremain {
			n = s.rremain
		}
		if _, err := s.rfile.Write(p[:n]); err != nil {
			return err
		}
		p = p[n:]
		s.rremain -= n
		if err := s.advance(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) finish() {
	h := s.rh
	if s.rgot != h.Total {
		s.logf("接收不完整：%d / %d 字节", s.rgot, h.Total)
		s.abortRecv()
		return
	}
	var tops []string
	if h.Kind == KindFiles {
		if s.rfile != nil {
			s.rfile.Close()
			s.rfile = nil
		}
		seen := map[string]bool{}
		for _, e := range h.Files {
			top := strings.SplitN(e.Path, string(filepath.Separator), 2)[0]
			if !seen[top] {
				seen[top] = true
				tops = append(tops, filepath.Join(s.rdir, top))
			}
		}
	}
	data := s.rbuf
	s.rh, s.rbuf, s.rdir = nil, nil, ""
	if s.OnClip != nil {
		s.OnClip(*h, data, tops)
	}
	if h.Kind == KindFiles {
		CleanOld(s.Dir, 3)
	}
}

func (s *Session) abortRecv() {
	if s.rfile != nil {
		s.rfile.Close()
		s.rfile = nil
	}
	if s.rdir != "" {
		os.RemoveAll(s.rdir)
	}
	s.rh, s.rbuf, s.rdir = nil, nil, ""
}

var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// sanitize 把对端给出的相对路径转成本机安全的相对路径，杜绝目录穿越。
func sanitize(wire string) (string, error) {
	parts := strings.Split(wire, "/")
	out := make([]string, 0, len(parts))
	for _, c := range parts {
		c = strings.Map(func(r rune) rune {
			if r < 0x20 || strings.ContainsRune(`<>:"|?*\`, r) {
				return '_'
			}
			return r
		}, c)
		c = strings.TrimRight(c, ". ")
		if c == "" || c == "." || c == ".." {
			return "", fmt.Errorf("非法路径 %q", wire)
		}
		base := c
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		if reserved[strings.ToUpper(base)] {
			c = "_" + c
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("非法路径 %q", wire)
	}
	return filepath.Join(out...), nil
}

// CleanOld 只保留最近的 keep 个接收目录。
func CleanOld(base string, keep int) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 目录名以毫秒时间戳开头
	for i := 0; i < len(names)-keep; i++ {
		os.RemoveAll(filepath.Join(base, names[i]))
	}
}

// HumanSize 把字节数格式化为可读字符串。
func HumanSize(n int64) string {
	const u = "KMGT"
	f := float64(n)
	if f < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	i := -1
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %cB", f, u[i])
}

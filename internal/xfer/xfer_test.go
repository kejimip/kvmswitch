package xfer

import (
	"context"
	"io/fs"
	"sync"

	"bytes"
	"crypto/rand"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kvmswitch/internal/proto"
)

type got struct {
	h     Header
	data  []byte
	paths []string
}

// pair 建立一对经过握手的回环连接。
func pair(t *testing.T, serverKey, clientKey string, ch byte) (*proto.Conn, *proto.Conn, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   *proto.Conn
		err error
	}
	sc := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			sc <- res{nil, err}
			return
		}
		pc, err := proto.Server(c, []byte(serverKey))
		sc <- res{pc, err}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cli, cerr := proto.Client(c, []byte(clientKey), ch)
	s := <-sc
	if cerr != nil {
		return nil, nil, cerr
	}
	if s.err != nil {
		return nil, nil, s.err
	}
	return s.c, cli, nil
}

func TestHandshakeWrongKey(t *testing.T) {
	if _, _, err := pair(t, "AAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBB", proto.ChanControl); err == nil {
		t.Fatal("密钥不一致时握手应当失败")
	}
}

func TestControlRoundTrip(t *testing.T) {
	s, c, err := pair(t, "KEYKEYKEYKEYKEYKEYKE", "KEYKEYKEYKEYKEYKEYKE", proto.ChanControl)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer c.Close()
	if s.Channel != proto.ChanControl {
		t.Fatalf("通道号 = %d", s.Channel)
	}
	for i := 0; i < 1000; i++ {
		if err := s.Write(proto.MsgMouseMove, []byte{0, 0, 0, byte(i), 0, 0, 0, 1}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 1000; i++ {
		typ, p, err := c.Read()
		if err != nil || typ != proto.MsgMouseMove || p[3] != byte(i) {
			t.Fatalf("第 %d 帧: typ=%d err=%v", i, typ, err)
		}
	}
}

func sessions(t *testing.T, maxAuto int64, accept bool) (send, recv *Session, ch chan got, recvDir string) {
	t.Helper()
	s, c, err := pair(t, "KEYKEYKEYKEYKEYKEYKE", "KEYKEYKEYKEYKEYKEYKE", proto.ChanData)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); c.Close() })
	recvDir = t.TempDir()
	ch = make(chan got, 4)
	send = &Session{C: s, Dir: t.TempDir(), MaxAuto: func() int64 { return maxAuto }}
	recv = &Session{C: c, Dir: recvDir, Logf: t.Logf,
		Confirm: func(Header) bool { return accept },
		OnClip:  func(h Header, d []byte, p []string) { ch <- got{h, d, p} }}
	go send.Run()
	go recv.Run()
	return
}

func wait(t *testing.T, ch chan got) got {
	t.Helper()
	select {
	case g := <-ch:
		return g
	case <-time.After(10 * time.Second):
		t.Fatal("等待接收超时")
	}
	return got{}
}

func TestTextAndImage(t *testing.T) {
	send, _, ch, _ := sessions(t, 0, true)
	text := []byte("你好，世界 hello\n第二行")
	if err := send.Send(1, Clip{Kind: KindText, Data: text}); err != nil {
		t.Fatal(err)
	}
	if g := wait(t, ch); g.h.Kind != KindText || !bytes.Equal(g.data, text) {
		t.Fatalf("文本不一致: %q", g.data)
	}
	big := make([]byte, 3*proto.ChunkSize+12345) // 跨多个分块
	rand.Read(big)
	if err := send.Send(2, Clip{Kind: KindImage, Data: big}); err != nil {
		t.Fatal(err)
	}
	if g := wait(t, ch); g.h.Kind != KindImage || !bytes.Equal(g.data, big) {
		t.Fatal("大块数据不一致")
	}
}

func makeTree(t *testing.T) (root string, tops []string, files map[string][]byte) {
	t.Helper()
	root = t.TempDir()
	files = map[string][]byte{
		"单个文件.txt":          []byte("single"),
		"空文件.bin":           {},
		"文件夹/a.txt":         []byte("aaa"),
		"文件夹/子目录/b.bin":     make([]byte, 700_000),
		"文件夹/子目录/empty.dat": {},
		"文件夹/子目录/更深/c.txt":  []byte("deep"),
	}
	rand.Read(files["文件夹/子目录/b.bin"])
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(root, "文件夹", "空目录"), 0o755)
	tops = []string{filepath.Join(root, "单个文件.txt"), filepath.Join(root, "空文件.bin"), filepath.Join(root, "文件夹")}
	return
}

func TestFiles(t *testing.T) {
	send, _, ch, recvDir := sessions(t, 0, true)
	_, tops, files := makeTree(t)
	if err := send.Send(7, Clip{Kind: KindFiles, Paths: tops, Cut: true}); err != nil {
		t.Fatal(err)
	}
	g := wait(t, ch)
	if g.h.Kind != KindFiles || !g.h.Cut || g.h.ID != 7 {
		t.Fatalf("头部不对: %+v", g.h)
	}
	if len(g.paths) != 3 {
		t.Fatalf("顶层条目应为 3 个，实际 %v", g.paths)
	}
	base := filepath.Dir(g.paths[0])
	if !strings.HasPrefix(base, recvDir) {
		t.Fatalf("接收目录不在缓存目录内: %s", base)
	}
	for rel, want := range files {
		b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("缺少 %s: %v", rel, err)
		}
		if !bytes.Equal(b, want) {
			t.Fatalf("%s 内容不一致", rel)
		}
	}
	if st, err := os.Stat(filepath.Join(base, "文件夹", "空目录")); err != nil || !st.IsDir() {
		t.Fatal("空目录没有被创建")
	}
}

func TestConfirmReject(t *testing.T) {
	send, _, ch, recvDir := sessions(t, 10, false) // 上限 10 字节，对端拒绝
	_, tops, _ := makeTree(t)
	if err := send.Send(1, Clip{Kind: KindFiles, Paths: tops}); err == nil {
		t.Fatal("对端拒绝时应返回错误")
	}
	select {
	case <-ch:
		t.Fatal("被拒绝的传输不应产生剪贴板内容")
	case <-time.After(300 * time.Millisecond):
	}
	if ents, _ := os.ReadDir(recvDir); len(ents) != 0 {
		t.Fatalf("被拒绝后不应留下文件: %v", ents)
	}
}

func TestConfirmAccept(t *testing.T) {
	send, _, ch, _ := sessions(t, 10, true)
	_, tops, _ := makeTree(t)
	if err := send.Send(1, Clip{Kind: KindFiles, Paths: tops}); err != nil {
		t.Fatal(err)
	}
	if g := wait(t, ch); !g.h.Confirm {
		t.Fatal("超过上限时应带确认标记")
	}
}

// 恶意对端不能通过路径把文件写到缓存目录之外。
func TestPathTraversalRejected(t *testing.T) {
	s, c, err := pair(t, "KEYKEYKEYKEYKEYKEYKE", "KEYKEYKEYKEYKEYKEYKE", proto.ChanData)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer c.Close()
	parent := t.TempDir()
	recvDir := filepath.Join(parent, "recv")
	ch := make(chan got, 1)
	recv := &Session{C: c, Dir: recvDir, OnClip: func(h Header, d []byte, p []string) { ch <- got{h, d, p} }}
	go recv.Run()
	for _, bad := range []string{"../evil.txt", "a/../../evil.txt", "/evil.txt", "..", "a//b"} {
		h := Header{ID: 1, Kind: KindFiles, Total: 4, Files: []FileEntry{{Path: bad, Size: 4}}}
		hb, _ := json.Marshal(h)
		s.Write(proto.MsgClipBegin, hb)
		s.Write(proto.MsgClipChunk, []byte("evil"))
		s.Write(proto.MsgClipEnd, nil)
	}
	select {
	case g := <-ch:
		t.Fatalf("非法路径被接受: %v", g.paths)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); err == nil {
		t.Fatal("文件被写到了缓存目录之外")
	}
}

func TestSanitize(t *testing.T) {
	ok := map[string]string{
		"a.txt":          "a.txt",
		"dir/b.txt":      filepath.Join("dir", "b.txt"),
		"a:b?.txt":       "a_b_.txt",
		"con.txt":        "_con.txt",
		"尾部空格. ":         "尾部空格",
		`back\slash.txt`: "back_slash.txt",
	}
	for in, want := range ok {
		if out, err := sanitize(in); err != nil || out != want {
			t.Errorf("sanitize(%q) = %q, %v；期望 %q", in, out, err, want)
		}
	}
}

func TestCleanOld(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"1000000000001-1", "1000000000002-2", "1000000000003-3", "1000000000004-4", "1000000000005-5"} {
		os.MkdirAll(filepath.Join(d, n), 0o755)
	}
	CleanOld(d, 3)
	ents, _ := os.ReadDir(d)
	if len(ents) != 3 || ents[0].Name() != "1000000000003-3" {
		t.Fatalf("清理结果不对: %v", ents)
	}
}

func TestUnreadableTreeFailsClosed(t *testing.T) {
	root := t.TempDir()
	_, _, _, err := walkList(context.Background(), []string{root}, func(path string, f fs.WalkDirFunc) error {
		return f(filepath.Join(path, "unreadable"), nil, fs.ErrPermission)
	})
	if err == nil {
		t.Fatal("unreadable entry silently omitted")
	}
}
func TestChangedFileRefused(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "changed")
	os.WriteFile(p, []byte("a"), 0600)
	_, files, _, err := buildList(context.Background(), []string{p})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("ab"), 0600)
	if err = (&Session{}).sendFile(context.Background(), files[0], make([]byte, 8)); err == nil {
		t.Fatal("changed source accepted")
	}
}

func TestCancelCleansPartialDirectory(t *testing.T) {
	send, recv, err := pair(t, "KEYKEYKEYKEYKEYKEYKE", "KEYKEYKEYKEYKEYKEYKE", proto.ChanData)
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()
	defer recv.Close()
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "large")
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(32 << 20); e != nil {
		t.Fatal(e)
	}
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	var once sync.Once
	s := &Session{C: recv, Dir: root, Progress: func(_ uint64, n, total int64) {
		if n < total {
			once.Do(cancel)
		}
	}, OnClip: func(h Header, _ []byte, _ []string) {
		if h.ID == 2 {
			close(finished)
		}
	}}
	go s.Run()
	tx := &Session{C: send}
	if err = tx.SendContext(ctx, 1, Clip{Kind: KindFiles, Paths: []string{path}}); err == nil {
		t.Fatal("expected cancellation")
	}
	if err = tx.Send(2, Clip{Kind: KindText, Data: []byte("barrier")}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("connection not reusable")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial files survived: %v %v", entries, err)
	}
}

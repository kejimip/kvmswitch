package filedrop

import (
	"bytes"
	"kvmswitch/internal/proto"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func wirePair(t *testing.T) (*proto.Conn, *proto.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	ch := make(chan *proto.Conn, 1)
	go func() { p, _ := proto.Server(b, []byte("test")); ch <- p }()
	client, e := proto.Client(a, []byte("test"), proto.ChanFiles)
	if e != nil {
		t.Fatal(e)
	}
	return client, <-ch
}
func TestTransferChecksumAndCollisions(t *testing.T) {
	a, b := wirePair(t)
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "blob")
	payload := bytes.Repeat([]byte{1, 2, 3, 4}, 140000)
	if err := os.WriteFile(src, payload, 0600); err != nil {
		t.Fatal(err)
	}
	files := []File{{"图.svg", int64(len(payload))}, {"图.svg", int64(len(payload))}}
	ch := make(chan error, 1)
	var saved int
	go func() {
		ch <- Receive(b, files, Settings{dir, 10, false}, func(i int, n int64, commit bool) {
			if commit {
				saved++
			}
		})
	}()
	if e := Send(a, files, []string{src, src}, func(int, int64, bool) {}); e != nil {
		t.Fatal(e)
	}
	if e := <-ch; e != nil || saved != 2 {
		t.Fatal(e, saved)
	}
	for _, name := range []string{"图.svg", "图 (1).svg"} {
		p, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil || !bytes.Equal(p, payload) {
			t.Fatal(name, e)
		}
	}
}
func TestBadChecksumKeepsNoPartial(t *testing.T) {
	a, b := wirePair(t)
	dir := t.TempDir()
	ch := make(chan error, 1)
	go func() { ch <- Receive(b, []File{{"x", 1}}, Settings{dir, 1, false}, func(int, int64, bool) {}) }()
	if e := wait(a, ready); e != nil {
		t.Fatal(e)
	}
	a.Write(chunk, []byte{1})
	a.Write(end, make([]byte, 32))
	if e := wait(a, ack); e == nil {
		t.Fatal("false receipt")
	}
	if <-ch == nil {
		t.Fatal("no error")
	}
	f, e := os.ReadDir(dir)
	if e != nil || len(f) != 0 {
		t.Fatal(f, e)
	}
}
func TestRejectManifest(t *testing.T) {
	s := Settings{t.TempDir(), 1, false}
	for _, f := range []File{{"../bad", 1}, {"X:foo", 0}, {"con.txt", 0}, {"a", -1}, {"x", 1<<20 + 1}} {
		if e := s.Check([]File{f}); e == nil {
			t.Fatal(f)
		}
	}
	s.Paused = true
	if e := s.Check([]File{{"ok", 0}}); e == nil {
		t.Fatal("accepted while paused")
	}
}

func TestDisconnectDuringReceive(t *testing.T) {
	a, b := wirePair(t)
	dir := t.TempDir()
	ch := make(chan error, 1)
	go func() {
		ch <- Receive(b, []File{{"partial.zip", 20}}, Settings{dir, 1, false}, func(int, int64, bool) {})
	}()
	if e := wait(a, ready); e != nil {
		t.Fatal(e)
	}
	if e := a.Write(chunk, []byte("only part")); e != nil {
		t.Fatal(e)
	}
	a.Close()
	if e := <-ch; e == nil {
		t.Fatal("unexpected success")
	}
	f, e := os.ReadDir(dir)
	if e != nil || len(f) != 0 {
		t.Fatalf("partial files: %v: %v", f, e)
	}
}

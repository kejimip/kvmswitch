package core

import (
	"bytes"
	"kvmswitch/internal/filedrop"
	"os"
	"path/filepath"
	"testing"
)

func TestIndependentFileTransfer(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	for _, c := range []*Core{r.win, r.mac} {
		s := *c.Config()
		s.FileDir = t.TempDir()
		s.FileLimitMiB = 2
		if e := c.ApplyConfig(&s); e != nil {
			t.Fatal(e)
		}
	}
	if r.win.Config().FileDir == r.mac.Config().FileDir {
		t.Fatal("receive path shared")
	}
	payload := bytes.Repeat([]byte("不修改剪贴板"), 32768)
	for _, x := range []struct{ src, dst *Core }{{r.win, r.mac}, {r.mac, r.win}} {
		dir := t.TempDir()
		path := filepath.Join(dir, "0")
		if e := os.WriteFile(path, payload, 0600); e != nil {
			t.Fatal(e)
		}
		if e := x.src.SendFiles([]filedrop.File{{Name: "资料.zip", Size: int64(len(payload))}}, []string{path}, dir, x.src.FilePeer()); e != nil {
			t.Fatal(e)
		}
		r.win.Hotkey(false)
		r.wait("send confirmed", func() bool {
			for _, j := range x.src.Files.Snapshot() {
				if j.Direction == "发送" && !j.Active && j.Completed == 1 {
					return true
				}
			}
			return false
		})
		b, e := os.ReadFile(filepath.Join(x.dst.Config().FileDir, "资料.zip"))
		if e != nil || !bytes.Equal(b, payload) {
			t.Fatal(e)
		}
	}
	for _, f := range []*fake{r.fw, r.fm} {
		f.get(func() {
			if len(f.written) != 0 || f.pasted != 0 {
				t.Fatal("manual transfer touched clipboard")
			}
		})
	}
}

func TestRetryAfterReceiverResumes(t *testing.T) {
	r := newRig(t, true)
	r.connected()
	s := *r.mac.Config()
	s.FileDir = t.TempDir()
	s.FilePaused = true
	if e := r.mac.ApplyConfig(&s); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "0")
	if e := os.WriteFile(p, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := r.win.SendFiles([]filedrop.File{{Name: "doc.md", Size: 1}}, []string{p}, dir, r.win.FilePeer()); e != nil {
		t.Fatal(e)
	}
	var id string
	r.wait("rejected with retry", func() bool {
		for _, j := range r.win.Files.Snapshot() {
			if j.Retry {
				id = j.ID
				return true
			}
		}
		return false
	})
	if _, e := os.Stat(filepath.Join(s.FileDir, "doc.md")); !os.IsNotExist(e) {
		t.Fatal("paused receiver wrote data")
	}
	s.FilePaused = false
	if e := r.mac.ApplyConfig(&s); e != nil {
		t.Fatal(e)
	}
	if e := r.win.FileAction(id, "retry"); e != nil {
		t.Fatal(e)
	}
	r.wait("retried batch confirmed", func() bool {
		for _, j := range r.win.Files.Snapshot() {
			if j.Status == "已送达" && j.Completed == 1 {
				return true
			}
		}
		return false
	})
}

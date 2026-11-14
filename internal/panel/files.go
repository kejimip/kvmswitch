package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"kvmswitch/internal/core"
	"kvmswitch/internal/filedrop"
	"net/http"
	"os"
	"path/filepath"
)

// Stream browser File objects into a private outgoing spool. The app owns the
// batch after this local handoff: hiding/closing the WebView cannot cancel it.
func manualRoutes(c *core.Core, h Host, post func(string, func(http.ResponseWriter, *http.Request))) {
	stage := make(chan struct{}, 1)
	post("/api/files/send", func(w http.ResponseWriter, r *http.Request) {
		select {
		case stage <- struct{}{}:
			defer func() { <-stage }()
		default:
			fail(w, 409, "正在提交另一批文件")
			return
		}
		if !c.State().PeerFiles || !c.State().Connected {
			fail(w, 409, "对端未连接或需要更新")
			return
		}
		session := r.Header.Get("X-KVM-Session")
		if session == "" || session != c.FilePeer() {
			fail(w, 409, "连接已变化，请重新点击发送")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, filedrop.MaxTotal+(2<<20))
		mr, e := r.MultipartReader()
		if e != nil {
			fail(w, 400, "需要文件列表")
			return
		}
		head, e := mr.NextPart()
		if e != nil || head.FormName() != "manifest" {
			fail(w, 400, "缺少清单")
			return
		}
		var files []filedrop.File
		b, e := io.ReadAll(io.LimitReader(head, (1<<20)+1))
		if e != nil || len(b) > 1<<20 || json.Unmarshal(b, &files) != nil {
			fail(w, 400, "无效清单")
			return
		}
		n, e := filedrop.Total(files)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		dir, e := os.MkdirTemp("", "kvmswitch-out-")
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		adopted := false
		defer func() {
			if !adopted {
				os.RemoveAll(dir)
			}
		}()
		if e = filedrop.Space(dir, n); e != nil {
			fail(w, 400, "发送暂存目录："+e.Error())
			return
		}
		var paths []string
		for i, f := range files {
			p, e := mr.NextPart()
			if e != nil || p.FormName() != "file" {
				fail(w, 400, "文件列表不完整")
				return
			}
			path := filepath.Join(dir, fmt.Sprint(i))
			out, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				fail(w, 500, e.Error())
				return
			}
			got, e := io.Copy(out, io.LimitReader(p, f.Size+1))
			ce := out.Close()
			if e != nil || ce != nil || got != f.Size {
				fail(w, 400, "暂存失败或文件大小已变")
				return
			}
			paths = append(paths, path)
		}
		if _, e = mr.NextPart(); e != io.EOF {
			fail(w, 400, "文件清单不匹配")
			return
		}
		if e = c.SendFiles(files, paths, dir, session); e != nil {
			fail(w, 409, e.Error())
			return
		}
		adopted = true
		writeJSON(w, 202, true)
	})
	post("/api/files/action", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID, Action string }
		if readBody(r, &req) != nil {
			fail(w, 400, "参数无效")
			return
		}
		if e := c.FileAction(req.ID, req.Action); e != nil {
			fail(w, 409, e.Error())
			return
		}
		writeJSON(w, 200, true)
	})
	post("/api/files/folder", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Action, ID string }
		if readBody(r, &req) != nil {
			fail(w, 400, "参数无效")
			return
		}
		host, ok := h.(interface {
			FileFolder(string, bool) (string, error)
		})
		if !ok {
			fail(w, 400, "平台不支持")
			return
		}
		dir := c.FileSettings().Dir
		if req.ID != "" {
			dir = ""
			for _, j := range c.Files.Snapshot() {
				if j.ID == req.ID && j.Direction == "接收" {
					dir = j.Dir
					break
				}
			}
		}
		if dir == "" || (req.Action != "pick" && req.Action != "open") {
			fail(w, 400, "未知目录")
			return
		}
		chosen, e := host.FileFolder(dir, req.Action == "pick")
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"dir": chosen})
	})
}

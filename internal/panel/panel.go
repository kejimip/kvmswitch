// Package panel 提供两端共用的操作面板：一个只供本机访问的网页界面及其接口。
// 把它显示成独立窗口的方式由各平台自己决定（Windows 用 Edge 应用窗口，Mac 用原生 WebView 窗口）。
package panel

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"kvmswitch/internal/core"
)

//go:embed panel.html
var page []byte

// Host 是面板需要的、与操作系统相关的能力。
type Host interface {
	Autostart() bool
	SetAutostart(on bool) error
	Open(what string) error // "log"、"config"、"recv"、"accessibility"
	Quit()
	MonitorInfo() string
	CopyText(s string) error
}

// LogRing 保存最近的日志行，供面板显示；可直接作为 log 的输出。
type LogRing struct {
	mu    sync.Mutex
	lines []string
	part  string
}

func (r *LogRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.part + string(p)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		r.lines = append(r.lines, strings.TrimRight(s[:i], "\r"))
		s = s[i+1:]
	}
	r.part = s
	if n := len(r.lines); n > 300 {
		r.lines = append([]string(nil), r.lines[n-300:]...)
	}
	return len(p), nil
}

func (r *LogRing) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.lines...)
}

type Server struct {
	addr    string // 127.0.0.1:端口
	token   string // 每次启动随机生成，接口调用必须携带
	testing atomic.Bool
}

// URL 返回面板地址；tab 为要定位到的页面，可为空。
func (s *Server) URL(tab string) string {
	u := fmt.Sprintf("http://%s/#t=%s", s.addr, s.token)
	if tab != "" {
		u += "&tab=" + tab
	}
	return u
}

type stateJSON struct {
	Version   string       `json:"version"`
	SelfOS    string       `json:"self_os"`
	SelfName  string       `json:"self_name"`
	Dialer    bool         `json:"dialer"` // 本机是主动发起连接的一端（可手动填写对方地址）
	Autostart bool         `json:"autostart"`
	State     core.State   `json:"state"`
	Config    *core.Config `json:"config"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

// Start 启动面板的本机 HTTP 服务。
func Start(c *core.Core, h Host, logs *LogRing, dialer bool) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tb := make([]byte, 16)
	rand.Read(tb)
	s := &Server{addr: ln.Addr().String(), token: hex.EncodeToString(tb)}
	if os.Getenv("KVM_DEBUG") != "" {
		log.Printf("面板地址 %s", s.URL(""))
	}

	api := http.NewServeMux()
	post := func(path string, fn func(w http.ResponseWriter, r *http.Request)) {
		api.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				fail(w, http.StatusMethodNotAllowed, "需要 POST")
				return
			}
			fn(w, r)
		})
	}
	ok := map[string]bool{"ok": true}

	api.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, stateJSON{
			Version: core.Version, SelfOS: c.P.OS(), SelfName: c.P.Hostname(), Dialer: dialer,
			Autostart: h.Autostart(), State: c.State(), Config: c.Config(),
		})
	})
	api.HandleFunc("/api/log", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"lines": logs.Snapshot()})
	})
	api.HandleFunc("/api/monitor/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"text": strings.ReplaceAll(h.MonitorInfo(), "\r\n", "\n")})
	})
	post("/api/toggle", func(w http.ResponseWriter, r *http.Request) {
		c.Toggle()
		writeJSON(w, 200, ok)
	})
	post("/api/config", func(w http.ResponseWriter, r *http.Request) {
		next := *c.Config() // 以当前配置为底，只覆盖提交的字段
		if err := readBody(r, &next); err != nil {
			fail(w, 400, "提交的内容格式有误")
			return
		}
		if err := c.ApplyConfig(&next); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "config": c.Config()})
	})
	post("/api/pair", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Key  string `json:"key"`
			Host string `json:"host"`
		}
		if readBody(r, &b) != nil {
			fail(w, 400, "提交的内容格式有误")
			return
		}
		if err := c.SetPairing(b.Key, b.Host); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "config": c.Config()})
	})
	post("/api/autostart", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			On bool `json:"on"`
		}
		if readBody(r, &b) != nil {
			fail(w, 400, "提交的内容格式有误")
			return
		}
		if err := h.SetAutostart(b.On); err != nil && b.On {
			fail(w, 500, "设置开机自启失败："+err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"autostart": h.Autostart()})
	})
	// 录制新热键：按键由输入线程捕获，所以与实际生效的判定完全一致
	post("/api/capture", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			On bool `json:"on"`
		}
		readBody(r, &b)
		if b.On {
			c.Keys.BeginCapture()
		} else {
			c.Keys.EndCapture()
		}
		writeJSON(w, 200, ok)
	})
	post("/api/capture/poll", func(w http.ResponseWriter, r *http.Request) {
		hk, got := c.Keys.PollCapture()
		switch {
		case !got:
			writeJSON(w, 200, map[string]any{"pending": true})
		case hk.VK == 0x1B && hk.Mods == 0:
			writeJSON(w, 200, map[string]any{"cancel": true})
		default:
			writeJSON(w, 200, map[string]any{"hotkey": core.FormatHotkey(hk)})
		}
	})
	post("/api/monitor/test", func(w http.ResponseWriter, r *http.Request) {
		if !s.testing.CompareAndSwap(false, true) {
			fail(w, 409, "测试正在进行中")
			return
		}
		defer s.testing.Store(false)
		backErr, err := c.MonitorTest()
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		res := map[string]any{"ok": true}
		if backErr != "" {
			res["back_error"] = backErr
		}
		writeJSON(w, 200, res)
	})
	post("/api/open", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			What string `json:"what"`
		}
		readBody(r, &b)
		if err := h.Open(b.What); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, ok)
	})
	post("/api/copy", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Text string `json:"text"`
		}
		if readBody(r, &b) != nil || b.Text == "" {
			fail(w, 400, "没有可复制的内容")
			return
		}
		if err := h.CopyText(b.Text); err != nil {
			fail(w, 500, "复制失败："+err.Error())
			return
		}
		writeJSON(w, 200, ok)
	})
	post("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ok)
		go h.Quit()
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只接受以本机地址访问的请求，防止其它网站通过 DNS 重绑定访问接口
		if r.Host != s.addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			tok := r.Header.Get("X-KVM-Token")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
				fail(w, http.StatusForbidden, "未授权")
				return
			}
			api.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:")
		w.Write(page)
	})
	go http.Serve(ln, handler)
	return s, nil
}

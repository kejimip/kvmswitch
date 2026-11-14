// Package update installs only hashed assets from this repository's latest stable release.
package update

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const repo = "kejimip/kvmswitch"
const apiURL = "https://api.github.com/repos/" + repo + "/releases/latest"
const maxPackage = 128 << 20

var semver = regexp.MustCompile("^v?(0|[1-9][0-9]{0,8})[.](0|[1-9][0-9]{0,8})[.](0|[1-9][0-9]{0,8})$")

func compare(a, b string) (int, error) {
	parse := func(s string) ([]int, error) {
		m := semver.FindStringSubmatch(s)
		if m == nil {
			return nil, fmt.Errorf("无效的稳定版本号：%q", s)
		}
		var v []int
		for _, p := range m[1:] {
			n, _ := strconv.Atoi(p)
			v = append(v, n)
		}
		return v, nil
	}
	x, e := parse(a)
	if e != nil {
		return 0, e
	}
	y, e := parse(b)
	if e != nil {
		return 0, e
	}
	return slices.CompareFunc(x, y, cmp.Compare[int]), nil
}

type Asset struct {
	Name, Digest, State string
	Size                int64
	URL                 string `json:"browser_download_url"`
}
type release struct {
	Tag               string `json:"tag_name"`
	Draft, Prerelease bool
	Assets            []Asset
}

func (r release) pick(current, platform string) (Asset, int, error) {
	n, e := compare(r.Tag, current)
	if e != nil {
		return Asset{}, 0, e
	}
	if !strings.HasPrefix(r.Tag, "v") || r.Draft || r.Prerelease {
		return Asset{}, 0, errors.New("不是正式发布版本")
	}
	if n < 0 {
		return Asset{}, n, nil
	}
	name := map[string]string{"windows": "KVMSwitch-Setup.msi", "mac": "KVMSwitch-mac.zip"}[platform]
	for _, a := range r.Assets {
		if a.Name != name {
			continue
		}
		want := "https://github.com/" + repo + "/releases/download/" + r.Tag + "/" + name
		hash, e := hex.DecodeString(strings.TrimPrefix(a.Digest, "sha256:"))
		if a.State != "uploaded" || a.URL != want || a.Size <= 0 || a.Size > maxPackage || !strings.HasPrefix(a.Digest, "sha256:") || e != nil || len(hash) != sha256.Size {
			return Asset{}, 0, errors.New("安装包的地址、大小或 SHA-256 元数据无效")
		}
		return a, n, nil
	}
	return Asset{}, 0, errors.New("该版本尚未发布本机安装包")
}

type State struct {
	Latest, Message  string
	Busy, Ready, New bool
}
type Manager struct {
	mu                sync.Mutex
	s                 State
	asset             Asset
	current, platform string
	quit              func()
	client            *http.Client
}

func Start(version, platform string, quit func()) *Manager {
	m := &Manager{current: version, platform: platform, quit: quit, client: &http.Client{Timeout: 4 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		h := req.URL.Hostname()
		if len(via) > 6 || req.URL.Scheme != "https" || !(h == "github.com" || strings.HasSuffix(h, ".githubusercontent.com")) {
			return errors.New("拒绝非 GitHub 下载重定向")
		}
		return nil
	}}}
	go func() {
		m.Check()
		for range time.NewTicker(12 * time.Hour).C {
			m.Check()
		}
	}()
	return m
}
func (m *Manager) State() State { m.mu.Lock(); defer m.mu.Unlock(); return m.s }
func (m *Manager) Check() bool {
	m.mu.Lock()
	if m.s.Busy {
		m.mu.Unlock()
		return false
	}
	m.s = State{Busy: true, Message: "正在检查 GitHub Releases…"}
	m.mu.Unlock()
	go func() {
		r, e := m.latest()
		var a Asset
		var n int
		if e == nil {
			a, n, e = r.pick(m.current, m.platform)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if e != nil {
			m.s = State{Message: "检查失败：" + e.Error()}
			return
		}
		msg := "已是最新版本"
		if n > 0 {
			msg = "发现新版本 " + r.Tag
		} else if n < 0 {
			msg = "本机版本高于最新发布版，不会降级"
		}
		m.asset = a
		m.s = State{Latest: r.Tag, Message: msg, Ready: n >= 0, New: n > 0}
	}()
	return true
}
func (m *Manager) latest() (r release, err error) {
	client := *m.client
	client.Timeout = 15 * time.Second
	res, e := client.Get(apiURL)
	if e != nil {
		return r, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return r, fmt.Errorf("GitHub: %s", res.Status)
	}
	e = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&r)
	return r, e
}
func verifiedCopy(w io.Writer, r io.Reader, a Asset) error {
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(w, h), io.LimitReader(r, a.Size+1))
	if e != nil {
		return e
	}
	if n != a.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != a.Digest {
		return errors.New("安装包大小或 SHA-256 不匹配，已停止安装")
	}
	return nil
}
func (m *Manager) Install(tag string) bool {
	m.mu.Lock()
	if m.s.Busy || !m.s.Ready || tag != m.s.Latest {
		m.mu.Unlock()
		return false
	}
	a := m.asset
	m.s.Busy = true
	m.s.Message = "正在下载并校验安装包…"
	m.mu.Unlock()
	go func() {
		e := m.downloadInstall(a)
		if e != nil {
			m.mu.Lock()
			m.s.Busy = false
			m.s.Message = "安装失败：" + e.Error()
			m.mu.Unlock()
			return
		}
		m.quit()
	}()
	return true
}
func (m *Manager) downloadInstall(a Asset) (err error) {
	dir, err := os.MkdirTemp("", "kvmswitch-update-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	res, e := m.client.Get(a.URL)
	if e != nil {
		var ue *url.Error
		if errors.As(e, &ue) {
			return ue.Err
		}
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("下载失败：%s", res.Status)
	}
	p := filepath.Join(dir, a.Name)
	f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	e = verifiedCopy(f, res.Body, a)
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return launch(p)
}

package filedrop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"

	"kvmswitch/internal/proto"
)

type Record struct {
	ID, Direction, Peer, Status, Name, Dir string
	Started                                int64
	Bytes, Total                           int64
	Index, Count, Completed                int
	Active, Retry                          bool
}
type Offer struct {
	ID    string
	Files []File
}

func NewID() string         { var b [16]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func ValidID(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 16 }

type job struct {
	Record
	files    []File
	paths    []string
	spool    string
	settings Settings
	pc       *proto.Conn
	timer    *time.Timer
}
type Manager struct {
	mu     sync.Mutex
	jobs   map[string]*job
	recent []Record
	retry  *job
}

func (m *Manager) Snapshot() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := make([]Record, 0, len(m.jobs)+len(m.recent))
	for _, j := range m.jobs {
		r = append(r, j.Record)
	}
	return append(r, m.recent...)
}
func (m *Manager) add(o Offer, dir, peer string, paths []string, spool string, s Settings) (*job, error) {
	n, e := Total(o.Files)
	if e != nil {
		return nil, e
	}
	if !ValidID(o.ID) {
		return nil, errors.New("无效的传输编号")
	}
	if m.jobs == nil {
		m.jobs = map[string]*job{}
	}
	for _, j := range m.jobs {
		if j.Direction == dir || j.ID == o.ID {
			return nil, errors.New("该方向已有传输进行中")
		}
	}
	j := &job{Record: Record{ID: o.ID, Direction: dir, Peer: peer, Status: "等待建立文件通道", Name: o.Files[0].Name, Started: time.Now().UnixMilli(), Total: n, Count: len(o.Files), Active: true, Dir: s.Dir}, files: o.Files, paths: paths, spool: spool, settings: s}
	m.jobs[o.ID] = j
	j.timer = time.AfterFunc(20*time.Second, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.jobs[o.ID] == j && j.pc == nil {
			m.finish(j, errors.New("文件通道连接超时"))
		}
	})
	return j, nil
}
func (m *Manager) Outgoing(o Offer, peer string, paths []string, spool string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, e := m.add(o, "发送", peer, paths, spool, Settings{})
	if e == nil {
		m.clearRetry()
	}
	return e
}
func (m *Manager) Incoming(o Offer, peer string, s Settings) error {
	if err := s.Check(o.Files); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, e := m.add(o, "接收", peer, nil, "", s)
	return e
}
func (m *Manager) clearRetry() {
	if m.retry != nil {
		os.RemoveAll(m.retry.spool)
		for i := range m.recent {
			m.recent[i].Retry = false
		}
		m.retry = nil
	}
}
func (m *Manager) Retry(id, peer string) (Offer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.retry
	if j == nil || j.ID != id || j.Peer != peer {
		return Offer{}, errors.New("这次传输无法重试，请重新选择文件")
	}
	o := Offer{NewID(), j.files[j.Completed:]}
	_, e := m.add(o, "发送", peer, j.paths[j.Completed:], j.spool, Settings{})
	if e != nil {
		return Offer{}, e
	}
	m.retry = nil
	for i := range m.recent {
		m.recent[i].Retry = false
	}
	return o, nil
}
func (m *Manager) finish(j *job, e error) {
	delete(m.jobs, j.ID)
	j.timer.Stop()
	j.Active = false
	if e == nil {
		j.Status = "已送达"
	} else {
		j.Status = e.Error()
	}
	if e != nil && j.Direction == "发送" && j.Completed < len(j.files) {
		m.clearRetry()
		m.retry = j
		j.Retry = true
	} else if j.spool != "" {
		os.RemoveAll(j.spool)
	}
	m.recent = append([]Record{j.Record}, m.recent...)
	if len(m.recent) > 30 {
		m.recent = m.recent[:30]
	}
}
func (m *Manager) Stop(id, msg string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return false
	}
	if j.pc != nil {
		j.pc.Close()
	}
	m.finish(j, errors.New(msg))
	return true
}
func (m *Manager) Disconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.pc != nil {
			j.pc.Close()
		}
		m.finish(j, errors.New("连接断开"))
	}
}

// Pairing changes invalidate staged bytes: never retry them against a different peer.
func (m *Manager) Forget() { m.Disconnect(); m.mu.Lock(); defer m.mu.Unlock(); m.clearRetry() }
func (m *Manager) Run(id string, pc *proto.Conn) {
	defer pc.Close()
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.pc != nil {
		m.mu.Unlock()
		return
	}
	j.pc = pc
	j.timer.Stop()
	m.mu.Unlock()
	var base int64
	progress := func(i int, n int64, commit bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.jobs[id] != j {
			return
		}
		j.Status = "传输中"
		j.Index = i + 1
		j.Name = j.files[i].Name
		j.Bytes = base + n
		if commit {
			base += n
			j.Completed++
		}
	}
	var err error
	if j.Direction == "发送" {
		err = Send(pc, j.files, j.paths, progress)
	} else {
		err = Receive(pc, j.files, j.settings, progress)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs[id] == j {
		m.finish(j, err)
	}
}

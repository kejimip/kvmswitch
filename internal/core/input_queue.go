package core

import (
	"kvmswitch/internal/proto"
	"sync"
	"time"
)

const queueSize = 512

type inputQueue struct {
	mu      sync.Mutex
	items   [queueSize]InputEvent
	head, n int
	wake    chan struct{}
}

func newInputQueue() inputQueue { return inputQueue{wake: make(chan struct{}, 1)} }
func (q *inputQueue) push(e InputEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e.Typ == proto.MsgMouseMove && q.n > 0 {
		last := &q.items[(q.head+q.n-1)%queueSize]
		if last.Typ == e.Typ {
			last.A += e.A
			last.B += e.B
			return
		}
	}
	if q.n >= queueSize-32 && (e.Typ == proto.MsgMouseMove || e.Typ == proto.MsgWheel) {
		return
	}
	if q.n == queueSize {
		q.head = 0
		q.n = 1
		q.items[0] = InputEvent{Typ: proto.MsgInputReset}
	}
	q.items[(q.head+q.n)%queueSize] = e
	q.n++
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
func (q *inputQueue) clear() { q.mu.Lock(); q.n = 0; q.head = 0; q.mu.Unlock() }
func (q *inputQueue) pop() (InputEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n == 0 {
		return InputEvent{}, false
	}
	e := q.items[q.head]
	q.head = (q.head + 1) % queueSize
	q.n--
	return e, true
}

type MediaResult struct {
	Key                       string
	Sent, Received, Listening bool
}
type mediaProbe struct {
	mu     sync.Mutex
	until  time.Time
	seq    uint64
	result MediaResult
}

func mediaName(v uint16) string {
	switch v {
	case 0xad:
		return "静音"
	case 0xae:
		return "音量 −"
	case 0xaf:
		return "音量 +"
	case 0xb0:
		return "下一首"
	case 0xb1:
		return "上一首"
	case 0xb3:
		return "播放 / 暂停"
	}
	return ""
}
func (c *Core) StartMediaTest() {
	p := &c.media
	p.mu.Lock()
	defer p.mu.Unlock()
	p.until = time.Now().Add(60 * time.Second)
	p.seq++
	p.result = MediaResult{}
}
func (c *Core) MediaState() MediaResult {
	p := &c.media
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.result
	r.Listening = time.Now().Before(p.until)
	return r
}
func (c *Core) LocalMedia(v uint16) { c.media.observe(v) }
func (p *mediaProbe) observe(v uint16) uint64 {
	name := mediaName(v)
	if name == "" {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !time.Now().Before(p.until) {
		return 0
	}
	p.seq++
	p.result = MediaResult{Key: name}
	return p.seq
}
func (p *mediaProbe) mark(seq uint64, ack bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if seq == p.seq {
		p.result.Sent = true
		if ack {
			p.result.Received = true
		}
	}
}

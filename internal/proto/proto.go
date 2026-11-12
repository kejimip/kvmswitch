// Package proto 定义两端之间的加密帧协议与消息类型。
//
// 握手：客户端发送 magic + 通道号 + 16 字节随机数，服务端回 16 字节随机数；
// 双方用预共享密钥经 HKDF-SHA256 派生出两个方向的 AES-256-GCM 密钥。
// 之后每一帧为 [4 字节密文长度][密文]，明文为 [1 字节类型][负载]。
package proto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	Magic   = "KVMS3"
	TCPPort = 47812
	UDPPort = 47813

	DiscoverRequest = "KVMSWITCH_DISCOVER_V1"
	DiscoverReply   = "KVMSWITCH_HERE_V1"

	ChanControl byte = 1
	ChanData    byte = 2

	MaxFrame  = 4 << 20
	ChunkSize = 256 << 10
)

// 消息类型
const (
	MsgHello byte = iota + 1
	MsgPing
	MsgPong

	// 控制通道（双向，两端对等）
	MsgConfig        // JSON：共享设置，两端以修改时间较新的为准
	MsgInfo          // JSON Info：设备名称、系统、是否可被控制
	MsgActive        // JSON Active：宣布键鼠现在归谁
	MsgDDCResult     // JSON DDCResult：本端尝试切换显示器的结果
	MsgKey           // [vk u16][scan u16][flags u8]，键码统一为 Windows 虚拟键码
	MsgMouseMove     // [dx i32][dy i32]
	MsgMouseButton   // [button u8][down u8]
	MsgWheel         // [dy i32][dx i32]，单位为 WHEEL_DELTA(120)
	MsgPasteDetected // [clip id u64]

	// 数据通道（双向）
	MsgClipBegin  // JSON ClipHeader
	MsgClipChunk  // 原始字节
	MsgClipEnd    // 空
	MsgClipAbort  // 空
	MsgClipAccept // [id u64]
	MsgClipReject // [id u64]
	MsgClipOffer
	MsgClipFetch
	MsgClipUnavailable
)

const (
	KeyFlagDown     = 1
	KeyFlagExtended = 2
)

// 鼠标按键编号
const (
	BtnLeft = iota
	BtnRight
	BtnMiddle
	BtnX1
	BtnX2
)

// Active 宣布键鼠的归属。Who 以发送方的视角填写。
type Active struct {
	Who     string `json:"who"`     // "sender"：归发送方；"receiver"：归接收方；"none"：各自独立
	Seq     uint64 `json:"seq"`     // 发起方的切换序号，用于对应显示器切换结果
	Monitor bool   `json:"monitor"` // 是否同时把显示器切到新的归属设备
}

type DDCResult struct {
	Seq uint64 `json:"seq"`
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

type Info struct {
	Name       string `json:"name"`
	OS         string `json:"os"` // "windows" 或 "mac"
	CanControl bool   `json:"can_control"`
	Version    string `json:"version"`
}

type Conn struct {
	c       net.Conn
	rd, wr  cipher.AEAD
	rn, wn  uint64
	wmu     sync.Mutex
	hdr     [4]byte
	Channel byte
}

func hkdf(secret, salt, info []byte) []byte {
	ext := hmac.New(sha256.New, salt)
	ext.Write(secret)
	prk := ext.Sum(nil)
	exp := hmac.New(sha256.New, prk)
	exp.Write(info)
	exp.Write([]byte{1})
	return exp.Sum(nil)
}

func newAEAD(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

func derive(key, cn, sn []byte, ch byte) (c2s, s2c cipher.AEAD) {
	salt := append(append(append([]byte{}, cn...), sn...), ch)
	return newAEAD(hkdf(key, salt, []byte("kvmswitch c2s"))), newAEAD(hkdf(key, salt, []byte("kvmswitch s2c")))
}

func tune(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		t.SetNoDelay(true)
		t.SetKeepAlive(true)
		t.SetKeepAlivePeriod(10 * time.Second)
	}
}

// Client 在已建立的 TCP 连接上完成客户端握手。
func Client(c net.Conn, key []byte, channel byte) (*Conn, error) {
	tune(c)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	cn := make([]byte, 16)
	rand.Read(cn)
	hello := append(append([]byte(Magic), channel), cn...)
	if _, err := c.Write(hello); err != nil {
		return nil, err
	}
	sn := make([]byte, 16)
	if _, err := io.ReadFull(c, sn); err != nil {
		return nil, err
	}
	pc := &Conn{c: c, Channel: channel}
	pc.wr, pc.rd = derive(key, cn, sn, channel)
	if err := pc.Write(MsgHello, nil); err != nil {
		return nil, err
	}
	t, _, err := pc.Read()
	if err != nil || t != MsgHello {
		return nil, errors.New("握手失败：配对密钥不一致")
	}
	return pc, nil
}

// Server 完成服务端握手，返回连接及其通道号。
func Server(c net.Conn, key []byte) (*Conn, error) {
	tune(c)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	buf := make([]byte, len(Magic)+1+16)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	if string(buf[:len(Magic)]) != Magic {
		return nil, errors.New("协议头不匹配")
	}
	ch := buf[len(Magic)]
	cn := buf[len(Magic)+1:]
	sn := make([]byte, 16)
	rand.Read(sn)
	if _, err := c.Write(sn); err != nil {
		return nil, err
	}
	pc := &Conn{c: c, Channel: ch}
	pc.rd, pc.wr = derive(key, cn, sn, ch)
	t, _, err := pc.Read()
	if err != nil || t != MsgHello {
		return nil, errors.New("握手失败：配对密钥不一致")
	}
	if err := pc.Write(MsgHello, nil); err != nil {
		return nil, err
	}
	return pc, nil
}

func nonce(n uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[4:], n)
	return b
}

// Write 可并发调用。
func (c *Conn) Write(typ byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	plain := make([]byte, 1+len(payload))
	plain[0] = typ
	copy(plain[1:], payload)
	out := make([]byte, 4, 4+len(plain)+16)
	out = c.wr.Seal(out, nonce(c.wn), plain, nil)
	c.wn++
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)-4))
	_, err := c.c.Write(out)
	return err
}

func (c *Conn) WriteJSON(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(typ, b)
}

// Read 只能由单个 goroutine 调用。
func (c *Conn) Read() (byte, []byte, error) {
	if _, err := io.ReadFull(c.c, c.hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(c.hdr[:])
	if n < 17 || n > MaxFrame {
		return 0, nil, fmt.Errorf("非法帧长度 %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.c, buf); err != nil {
		return 0, nil, err
	}
	plain, err := c.rd.Open(buf[:0], nonce(c.rn), buf, nil)
	if err != nil {
		return 0, nil, errors.New("解密失败")
	}
	c.rn++
	return plain[0], plain[1:], nil
}

func (c *Conn) SetReadDeadline(t time.Time) { c.c.SetReadDeadline(t) }
func (c *Conn) Close() error                { return c.c.Close() }
func (c *Conn) RemoteAddr() net.Addr        { return c.c.RemoteAddr() }

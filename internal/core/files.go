package core

import (
	"encoding/json"
	"errors"
	"kvmswitch/internal/filedrop"
	"kvmswitch/internal/proto"
	"net"
	"time"
)

type fileReply struct{ ID, Reason string }

func (c *Core) FileSettings() filedrop.Settings {
	s := c.Config()
	return filedrop.Settings{Dir: s.FileDir, LimitMiB: s.FileLimitMiB, Paused: s.FilePaused}
}
func (c *Core) SendFiles(files []filedrop.File, paths []string, spool string, expected string) error {
	res := make(chan error, 1)
	c.do(func() {
		if c.ctrl == nil || !c.peer.Files || c.FilePeer() != expected {
			res <- errors.New("对端离线、配对连接已变化或版本不支持文件传输")
			return
		}
		o := filedrop.Offer{ID: filedrop.NewID(), Files: files}
		if e := c.Files.Outgoing(o, c.peer.Name, paths, spool); e != nil {
			res <- e
			return
		}
		// The manager owns the spool now, including retries after a write failure.
		if e := c.ctrl.WriteJSON(proto.MsgFilesOffer, o); e != nil {
			c.Files.Stop(o.ID, e.Error())
		}
		res <- nil
	})
	return <-res
}
func (c *Core) FilePeer() string { return c.State().FileSession }
func (c *Core) FileAction(id, act string) error {
	res := make(chan error, 1)
	c.do(func() {
		if c.ctrl == nil || !c.peer.Files {
			res <- errors.New("对端未连接或版本不兼容")
			return
		}
		switch act {
		case "cancel":
			if !c.Files.Stop(id, "已取消") {
				res <- errors.New("传输已结束")
				return
			}
			res <- c.ctrl.WriteJSON(proto.MsgFilesStop, fileReply{id, "对端已取消"})
		case "retry":
			o, e := c.Files.Retry(id, c.peer.Name)
			if e == nil {
				e = c.ctrl.WriteJSON(proto.MsgFilesOffer, o)
				if e != nil {
					c.Files.Stop(o.ID, e.Error())
				}
			}
			res <- e
		default:
			res <- errors.New("未知操作")
		}
	})
	return <-res
}

// A fresh authenticated socket per batch. The dialer always initiates it,
// independently of which end sent the offer; the random ID binds it to control.
func (c *Core) fileControl(t byte, p []byte) {
	switch t {
	case proto.MsgFilesOffer:
		var o filedrop.Offer
		if len(p) > 1<<20 || json.Unmarshal(p, &o) != nil || !filedrop.ValidID(o.ID) {
			return
		}
		if e := c.Files.Incoming(o, c.peer.Name, c.FileSettings()); e != nil {
			c.ctrl.WriteJSON(proto.MsgFilesStop, fileReply{o.ID, e.Error()})
			return
		}
		if c.ctrl.WriteJSON(proto.MsgFilesAccept, fileReply{ID: o.ID}) != nil {
			return
		}
		if !c.opt.Listen {
			c.dialFiles(o.ID)
		}
	case proto.MsgFilesAccept, proto.MsgFilesStop:
		var r fileReply
		if len(p) > 4096 || json.Unmarshal(p, &r) != nil || !filedrop.ValidID(r.ID) {
			return
		}
		if t == proto.MsgFilesStop {
			c.Files.Stop(r.ID, r.Reason)
		} else if !c.opt.Listen {
			c.dialFiles(r.ID)
		}
	}
}
func (c *Core) dialFiles(id string) {
	addr, key := c.ctrl.RemoteAddr().String(), *c.keyp.Load()
	go func() {
		conn, e := net.DialTimeout("tcp", addr, 5*time.Second)
		if e == nil {
			var pc *proto.Conn
			pc, e = proto.Client(conn, key, proto.ChanFiles)
			if e == nil {
				e = pc.Write(proto.MsgHello, []byte(id))
				if e == nil {
					c.Files.Run(id, pc)
					return
				}
			}
			conn.Close()
		}
		c.Files.Stop(id, "文件通道连接失败："+e.Error())
	}()
}
func (c *Core) acceptFiles(pc *proto.Conn) {
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	t, id, e := pc.Read()
	if e != nil || t != proto.MsgHello || !filedrop.ValidID(string(id)) {
		pc.Close()
		return
	}
	c.Files.Run(string(id), pc)
}

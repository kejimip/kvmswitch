// Package filedrop implements manual, clipboard-independent transfers.
package filedrop

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"kvmswitch/internal/proto"
)

type File struct {
	Name string
	Size int64
}

const MaxTotal int64 = 1 << 40

func Total(files []File) (int64, error) {
	if len(files) == 0 || len(files) > 1024 {
		return 0, errors.New("每批请选择 1–1024 个普通文件")
	}
	var total int64
	for _, f := range files {
		n := f.Name
		stem := strings.ToUpper(strings.TrimRight(strings.SplitN(n, ".", 2)[0], " "))
		reserved := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" || ((strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && len([]rune(stem)) == 4 && strings.ContainsRune("0123456789¹²³", []rune(stem)[3]))
		if n == "" || !utf8.ValidString(n) || len(n) > 255 || strings.ContainsAny(n, "/\\:*?\"<>|") || strings.IndexFunc(n, unicode.IsControl) >= 0 || strings.HasSuffix(n, ".") || strings.HasSuffix(n, " ") || reserved || !filepath.IsLocal(n) {
			return 0, fmt.Errorf("文件名不适合跨平台保存：%q", n)
		}
		if f.Size < 0 || f.Size > MaxTotal-total {
			return 0, errors.New("单批最大支持 1 TiB")
		}
		total += f.Size
	}
	return total, nil
}

type Settings struct {
	Dir      string
	LimitMiB int64
	Paused   bool
}

func (s Settings) Validate() error {
	if !filepath.IsAbs(s.Dir) || s.LimitMiB < 1 || s.LimitMiB > MaxTotal>>20 {
		return errors.New("请输入本机绝对目录，接收上限为 1–1048576 MiB")
	}
	return nil
}
func Space(dir string, need int64) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	free, err := available(dir)
	if err != nil {
		return err
	}
	if free < uint64(need) {
		return errors.New("可用磁盘空间不足")
	}
	return nil
}
func (s Settings) Check(files []File) error {
	if s.Paused {
		return errors.New("接收端已暂停自动接收")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	n, err := Total(files)
	if err != nil {
		return err
	}
	if n > s.LimitMiB<<20 {
		return fmt.Errorf("超过接收端单批上限 %d MiB", s.LimitMiB)
	}
	return Space(s.Dir, n)
}

const (
	chunk byte = iota + 1
	end
	ack
	problem
	ready
)

type Progress func(index int, bytes int64, committed bool)

func read(pc *proto.Conn) (byte, []byte, error) {
	pc.SetReadDeadline(time.Now().Add(60 * time.Second))
	t, b, e := pc.Read()
	if e == nil && t == problem {
		return 0, nil, fmt.Errorf("对端：%.500s", b)
	}
	return t, b, e
}
func wait(pc *proto.Conn, want byte) error {
	t, _, e := read(pc)
	if e == nil && t != want {
		return errors.New("无效的文件传输响应")
	}
	return e
}
func Send(pc *proto.Conn, files []File, paths []string, progress Progress) error {
	if len(files) != len(paths) {
		return errors.New("待发送文件列表不完整")
	}
	if err := wait(pc, ready); err != nil {
		return err
	}
	buf := make([]byte, proto.ChunkSize)
	for i, f := range files {
		err := func() error {
			r, e := os.Open(paths[i])
			if e != nil {
				return e
			}
			defer r.Close()
			h := sha256.New()
			var done int64
			progress(i, 0, false)
			for done < f.Size {
				n, e := io.ReadFull(r, buf[:min(int64(len(buf)), f.Size-done)])
				if e != nil {
					return e
				}
				h.Write(buf[:n])
				if e = pc.Write(chunk, buf[:n]); e != nil {
					return e
				}
				done += int64(n)
				progress(i, done, false)
			}
			if e = pc.Write(end, h.Sum(nil)); e != nil {
				return e
			}
			return wait(pc, ack)
		}()
		if err != nil {
			return err
		}
		progress(i, f.Size, true)
	}
	return nil
}
func Receive(pc *proto.Conn, files []File, s Settings, progress Progress) (err error) {
	defer func() {
		if err != nil {
			_ = pc.Write(problem, []byte(err.Error()))
		}
	}()
	if err = s.Check(files); err != nil {
		return err
	}
	if err = pc.Write(ready, nil); err != nil {
		return err
	}
	for i, f := range files {
		err = func() error {
			w, e := os.CreateTemp(s.Dir, ".kvmswitch-*.part")
			if e != nil {
				return e
			}
			tmp := w.Name()
			defer os.Remove(tmp)
			defer w.Close()
			h := sha256.New()
			var done int64
			progress(i, 0, false)
			for {
				t, b, e := read(pc)
				if e != nil {
					return e
				}
				switch t {
				case chunk:
					if len(b) == 0 || len(b) > proto.ChunkSize || int64(len(b)) > f.Size-done {
						return errors.New("文件长度不符")
					}
					if _, e = w.Write(b); e != nil {
						return e
					}
					h.Write(b)
					done += int64(len(b))
					progress(i, done, false)
				case end:
					if done != f.Size || subtle.ConstantTimeCompare(h.Sum(nil), b) != 1 {
						return errors.New("文件大小或 SHA-256 校验失败")
					}
					if e = w.Sync(); e != nil {
						return e
					}
					if e = w.Close(); e != nil {
						return e
					}
					if e = publish(tmp, s.Dir, f.Name); e != nil {
						return e
					}
					progress(i, done, true)
					return pc.Write(ack, nil)
				default:
					return errors.New("非法文件数据帧")
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
func publish(tmp, dir, name string) error {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		err := moveNew(tmp, filepath.Join(dir, n))
		if !os.IsExist(err) {
			return err
		}
	}
	return errors.New("同名文件过多")
}

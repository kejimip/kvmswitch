package main

import (
	"os"
	"os/exec"
	"strings"
)

func (*macPlatform) FileFolder(dir string, pick bool) (string, error) {
	if !pick {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return "", e
		}
		return dir, exec.Command("/usr/bin/open", dir).Start()
	}
	out, e := exec.Command("/usr/bin/osascript", "-e", `try
 return POSIX path of (choose folder with prompt "选择 KVMSwitch 接收目录")
 on error number -128
 return ""
 end try`).Output()
	return strings.TrimSpace(string(out)), e
}

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func (*winPlatform) FileFolder(dir string, pick bool) (string, error) {
	if !pick {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return "", e
		}
		shellOpen(dir)
		return dir, nil
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", `[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false); Add-Type -AssemblyName System.Windows.Forms; $d=New-Object Windows.Forms.FolderBrowserDialog; $d.SelectedPath=$env:KVM_PICK_DIR; if ($d.ShowDialog() -eq 'OK'){[Console]::Write($d.SelectedPath)}; $d.Dispose()`)
	cmd.Env = append(os.Environ(), "KVM_PICK_DIR="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, e := cmd.Output()
	return strings.TrimSpace(string(out)), e
}

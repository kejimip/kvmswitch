package update

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Stage on the target volume first. Only exit the running app after extraction
// succeeds; keep the previous bundle for recovery if opening the new app fails.
func launch(pkg string) (err error) {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	suffix := "/Contents/MacOS/kvmswitch-mac"
	if !strings.HasSuffix(exe, suffix) {
		return errors.New("请先将 KVMSwitch.app 安装到可写的应用程序目录")
	}
	app := strings.TrimSuffix(exe, suffix)
	if filepath.Base(app) != "KVMSwitch.app" {
		return errors.New("应用包路径不匹配")
	}
	stage, e := os.MkdirTemp(filepath.Dir(app), ".kvmswitch-update-")
	if e != nil {
		return e
	}
	defer func() {
		if err != nil {
			os.RemoveAll(stage)
		}
	}()
	if e = exec.Command("/usr/bin/ditto", "-x", "-k", pkg, stage).Run(); e != nil {
		return e
	}
	bin := filepath.Join(stage, "KVMSwitch.app", "Contents", "MacOS", "kvmswitch-mac")
	st, e := os.Lstat(bin)
	if e != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 {
		return errors.New("更新包没有可执行的 Mac 应用")
	}
	// Check the extracted bundle before quitting or touching the installed copy.
	if msg, e := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", filepath.Join(stage, "KVMSwitch.app")).CombinedOutput(); e != nil {
		return fmt.Errorf("更新包签名完整性检查失败，已保留当前版本：%w %s", e, strings.TrimSpace(string(msg)))
	}
	// Do not tie the installer to the terminal/session of the app being replaced.
	logDir, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	logFile, e := os.OpenFile(filepath.Join(logDir, "Library", "Logs", "KVMSwitch-update.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer logFile.Close()
	cmd := exec.Command("/bin/bash", "-c", macInstallScript, "kvm-update", strconv.Itoa(os.Getpid()), app, stage, pkg)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Dir = stage
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

//go:embed install_mac.sh
var macInstallScript string

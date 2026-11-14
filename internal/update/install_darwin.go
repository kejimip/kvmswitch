package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	script := `set -eu
pid=$1; app=$2; stage=$3
for i in {1..300}; do ! kill -0 "$pid" 2>/dev/null && break; sleep 0.1; done
if kill -0 "$pid" 2>/dev/null; then exit 1; fi
/bin/mv "$app" "$stage/previous.app"
if ! /bin/mv "$stage/KVMSwitch.app" "$app"; then /bin/mv "$stage/previous.app" "$app"; exit 1; fi
if ! /usr/bin/open "$app"; then
 /bin/mv "$app" "$stage/failed.app"
 /bin/mv "$stage/previous.app" "$app"
 /usr/bin/open "$app"; exit 1
fi
/bin/rm -rf "$stage"
/bin/rm -f "$4"
`
	return exec.Command("/bin/bash", "-c", script, "kvm-update", strconv.Itoa(os.Getpid()), app, stage, pkg).Start()
}

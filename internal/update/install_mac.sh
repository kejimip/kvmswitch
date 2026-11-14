# Executed by /bin/bash in the logged-in user's session (not as root).
set -eu
pid=$1; app=$2; stage=$3; pkg=$4
log() { printf '%s %s\n' "$(date '+%F %T')" "$*"; }
log "等待 KVMSwitch 退出，准备更新"
for ((i=0; i<300; i++)); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
done
if kill -0 "$pid" 2>/dev/null; then log "旧进程未退出，已取消替换"; exit 1; fi
/bin/mv "$app" "$stage/previous.app"
if ! /bin/mv "$stage/KVMSwitch.app" "$app"; then
    /bin/mv "$stage/previous.app" "$app"
    log "替换失败，已还原，未启动"; exit 1
fi
# Verify the final path, not just the extracted candidate. Short-circuit:
# LaunchServices is never called if this check fails.
if ! /usr/bin/codesign --verify --deep --strict "$app" || ! /usr/bin/open -n "$app"; then
    /bin/mv "$app" "$stage/failed.app"
    /bin/mv "$stage/previous.app" "$app"
    log "校验或启动请求失败，已还原，不启动旧版"; exit 1
fi
log "安装成功，已请求打开新版"
cd /
/bin/rm -rf "$stage"
/bin/rm -f "$pkg"

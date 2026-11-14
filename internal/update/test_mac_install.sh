#!/usr/bin/env bash
# Exercise the production helper on disposable bundles; stub Apple-only commands.
set -eu
src=$(cd "$(dirname "$0")" && pwd)/install_mac.sh
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
for scenario in success missing invalid blocked; do
    d="$root/$scenario"; app="$d/KVMSwitch.app"; stage="$d/stage"
    mkdir -p "$app" "$stage"; touch "$app/old" "$d/pkg"
    if [ "$scenario" != missing ]; then mkdir "$stage/KVMSwitch.app"; touch "$stage/KVMSwitch.app/new"; fi
    verify() { test -f "$app/new" && test "$scenario" != invalid; }
    launch_app() { echo attempt >> "$d/opens"; test "$1" = -n && test "$2" = "$app" && test "$scenario" != blocked; }
    export -f verify launch_app
    export app d scenario
    script=$(sed -e 's@/usr/bin/codesign @verify @' -e 's@/usr/bin/open @launch_app @' "$src")
    rc=0; bash -c "$script" kvm-update 2147483647 "$app" "$stage" "$d/pkg" || rc=$?
    if [ "$scenario" = success ]; then
        test "$rc" = 0 && test -f "$app/new" && test ! -e "$stage"
    else
        test "$rc" != 0 && test -f "$app/old"
    fi || exit 1
    case "$scenario" in
        success|blocked) test "$(wc -l < "$d/opens")" -eq 1;;
        *) test ! -e "$d/opens";;
    esac
done

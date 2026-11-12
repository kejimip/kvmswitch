// pack 把交叉编译出的 Mac 可执行文件打包成 KVMSwitch.app，并压缩为 zip。
// 之所以用 zip：在 Windows 上无法设置“可执行”权限，而 zip 可以把权限记录在压缩包里，
// 到 Mac 上双击解压后就是一个能直接运行的应用。
//
// 用法：go run ./tools/pack <Mac 可执行文件> <输出 zip> <版本号>
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const infoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>KVMSwitch</string>
	<key>CFBundleDisplayName</key><string>KVMSwitch</string>
	<key>CFBundleIdentifier</key><string>com.kvmswitch.app</string>
	<key>CFBundleExecutable</key><string>kvmswitch-mac</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleShortVersionString</key><string>VERSION</string>
	<key>CFBundleVersion</key><string>VERSION</string>
	<key>LSMinimumSystemVersion</key><string>12.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
	<key>NSAppTransportSecurity</key>
	<dict>
		<key>NSAllowsLocalNetworking</key><true/>
	</dict>
	<key>NSLocalNetworkUsageDescription</key><string>KVMSwitch 需要在局域网内找到并连接你的另一台设备。</string>
</dict>
</plist>
`

func main() {
	if len(os.Args) != 4 {
		fmt.Println("用法：pack <Mac 可执行文件> <输出 zip> <版本号>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Println("打包失败:", err)
		os.Exit(1)
	}
}

func run(bin, out, version string) error {
	src, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	now := time.Now()

	dir := func(name string) error {
		h := &zip.FileHeader{Name: name, Modified: now}
		h.SetMode(os.ModeDir | 0o755)
		_, err := zw.CreateHeader(h)
		return err
	}
	file := func(name string, mode os.FileMode, r io.Reader) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		return err
	}
	for _, d := range []string{"KVMSwitch.app/", "KVMSwitch.app/Contents/", "KVMSwitch.app/Contents/MacOS/"} {
		if err := dir(d); err != nil {
			return err
		}
	}
	plist := strings.ReplaceAll(infoPlist, "VERSION", version)
	if err := file("KVMSwitch.app/Contents/Info.plist", 0o644, strings.NewReader(plist)); err != nil {
		return err
	}
	if err := file("KVMSwitch.app/Contents/MacOS/kvmswitch-mac", 0o755, src); err != nil {
		return err
	}
	return zw.Close()
}

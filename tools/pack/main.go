// pack signs the complete macOS bundle before archiving it with Unix modes.
// 用法：go run ./tools/pack <Mac 可执行文件> <输出 zip> <版本号>
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	stage, err := os.MkdirTemp("", "kvmswitch-pack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	app := filepath.Join(stage, "KVMSwitch.app")
	contents := filepath.Join(app, "Contents")
	if err := os.MkdirAll(filepath.Join(contents, "MacOS"), 0755); err != nil {
		return err
	}
	exe, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(contents, "MacOS", "kvmswitch-mac"), exe, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(strings.ReplaceAll(infoPlist, "VERSION", version)), 0644); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		if err := command("/usr/bin/codesign", "--force", "--sign", "-", app); err != nil {
			return err
		}
		if err := command("/usr/bin/codesign", "--verify", "--deep", "--strict", app); err != nil {
			return err
		}
	} else {
		signer := os.Getenv("RCODESIGN")
		if signer == "" {
			signer = "rcodesign"
		}
		if err := command(signer, "sign", app); err != nil {
			return fmt.Errorf("需要 rcodesign 对完整 Mac 应用签名（可设置 RCODESIGN 路径）: %w", err)
		}
	}
	if info, err := os.Stat(filepath.Join(contents, "_CodeSignature", "CodeResources")); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("签名未生成 CodeResources")
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".kvmswitch-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = archive(f, stage, app)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), out)
}

func command(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func archive(f io.Writer, stage, app string) error {
	zw := zip.NewWriter(f)
	err := filepath.WalkDir(app, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: filepath.ToSlash(rel), Method: zip.Deflate, Modified: time.Now()}
		if d.IsDir() {
			h.Name += "/"
			h.SetMode(os.ModeDir | 0755)
			_, err := zw.CreateHeader(h)
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unexpected bundle entry: %s", rel)
		}
		if h.Name == "KVMSwitch.app/Contents/MacOS/kvmswitch-mac" {
			h.SetMode(0755)
		} else {
			h.SetMode(0644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

package main

import (
	webview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
)

func runPanelWindow() int {
	runtime.LockOSThread()
	raw := os.Getenv("KVMSWITCH_PANEL_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Port() == "" {
		return 2
	}
	w := webview.NewWithOptions(webview.WebViewOptions{DataPath: filepath.Join(dataDir(), "WebView2"), AutoFocus: true, WindowOptions: webview.WindowOptions{Title: panelWinTitle, Width: 1060, Height: 760, Center: true, IconId: 1}})
	if w == nil {
		return 1
	}
	defer w.Destroy()
	// A dead parent must not leave a window with an expired panel token around.
	if parent, e := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(os.Getppid())); e == nil {
		go func() {
			defer windows.CloseHandle(parent)
			windows.WaitForSingleObject(parent, windows.INFINITE)
			w.Dispatch(w.Terminate)
		}()
	}
	w.Navigate(raw)
	w.Run()
	return 0
}

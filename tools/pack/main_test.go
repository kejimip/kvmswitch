package main

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Exercise the final ZIP, not just the signing tool's staging directory.
func TestSignedArchive(t *testing.T) {
	if runtime.GOOS != "darwin" && os.Getenv("RCODESIGN") == "" {
		if _, err := exec.LookPath("rcodesign"); err != nil {
			t.Skip("rcodesign not installed")
		}
	}
	tmp := t.TempDir()
	mainFile := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bin, out := filepath.Join(tmp, "input"), filepath.Join(tmp, "out.zip")
	build := exec.Command("go", "build", "-o", bin, mainFile)
	build.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
	if msg, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, msg)
	}
	if err := run(bin, out, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	z, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	for member, perm := range map[string]os.FileMode{
		"Info.plist": 0644, "MacOS/kvmswitch-mac": 0755, "_CodeSignature/CodeResources": 0644,
	} {
		entry := false
		for _, f := range z.File {
			if f.Name == "KVMSwitch.app/Contents/"+member && f.Mode().Perm() == perm && f.UncompressedSize64 > 0 {
				entry = true
			}
		}
		if !entry {
			t.Fatalf("missing or invalid archive entry: %s", member)
		}
	}
	if runtime.GOOS == "darwin" {
		if err := command("/usr/bin/ditto", "-x", "-k", out, tmp); err != nil {
			t.Fatal(err)
		}
		if err := command("/usr/bin/codesign", "--verify", "--deep", "--strict", filepath.Join(tmp, "KVMSwitch.app")); err != nil {
			t.Fatal(err)
		}
	}
}

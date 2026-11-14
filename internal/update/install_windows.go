package update

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"path/filepath"
)

// Normal MSI UI allows the user to cancel or select Repair. Keep the source MSI
// available to Windows Installer after launch; do not delete it while in use.
func launch(path string) error {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	return exec.Command(filepath.Join(dir, "msiexec.exe"), "/i", path).Start()
}

package filedrop

import "golang.org/x/sys/windows"

func available(dir string) (uint64, error) {
	p, e := windows.UTF16PtrFromString(dir)
	if e != nil {
		return 0, e
	}
	var n uint64
	e = windows.GetDiskFreeSpaceEx(p, &n, nil, nil)
	return n, e
}
func moveNew(a, b string) error {
	x, e := windows.UTF16PtrFromString(a)
	if e != nil {
		return e
	}
	y, e := windows.UTF16PtrFromString(b)
	if e != nil {
		return e
	}
	return windows.MoveFile(x, y)
}

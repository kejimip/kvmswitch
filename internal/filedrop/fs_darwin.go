package filedrop

import "golang.org/x/sys/unix"

func moveNew(a, b string) error { return unix.RenamexNp(a, b, unix.RENAME_EXCL) }

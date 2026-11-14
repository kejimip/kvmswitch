//go:build !darwin && !windows

package filedrop

import "os"

func moveNew(a, b string) error { return os.Link(a, b) }

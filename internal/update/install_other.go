//go:build !darwin && !windows

package update

import "errors"

func launch(string) error { return errors.New("不支持此系统") }

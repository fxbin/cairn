//go:build darwin || linux

// sync_unix.go 在 Unix 平台同步目录元数据，使原子重命名后的制品可靠落盘。
//
// Author: fxbin
package artifact

import (
	"fmt"
	"os"
)

// syncDirectory 同步目录元数据。
func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open artifact directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync artifact directory: %w", err)
	}
	return nil
}

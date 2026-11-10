//go:build !linux

package agent

import "os"

// fileOwner is only supported on Linux; on other platforms it returns "".
func fileOwner(info os.FileInfo) string { return "" }

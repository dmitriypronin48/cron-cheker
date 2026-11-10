//go:build linux

package agent

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// fileOwner returns the user name owning the file, resolved from the stat
// uid. Falls back to the numeric uid string; returns "" if not available.
func fileOwner(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	u, err := user.LookupId(uid)
	if err != nil {
		return uid
	}
	return u.Username
}

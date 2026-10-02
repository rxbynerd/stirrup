//go:build unix

package judge

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// fileOwner returns the uid that owns the file info describes.
var fileOwner = func(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// checkPrivateDir refuses a directory that the current user does not own
// or that its group or other users can write.
func checkPrivateDir(info fs.FileInfo) error {
	uid, ok := fileOwner(info)
	if !ok {
		return fmt.Errorf("cannot read the owner of %s", info.Name())
	}
	if euid := os.Geteuid(); uid != euid {
		return fmt.Errorf("owned by uid %d, not the current user (uid %d)", uid, euid)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("mode %04o lets other users write it; remove group and other write permission (chmod go-w)", uint32(perm))
	}
	return nil
}

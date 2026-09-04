//go:build unix

package bots

import (
	"fmt"
	"os"
	"syscall"
)

// statOwner reads uid/gid/mode without following the file's content. It is the
// backbone of the "the import changed nothing" assertion in TAKAN_BOTS.md §9.
func statOwner(path string) (OwnerStat, error) {
	st, err := os.Stat(path)
	if err != nil {
		return OwnerStat{}, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return OwnerStat{}, fmt.Errorf("%s: no unix stat available", path)
	}
	return OwnerStat{Path: path, UID: uint32(sys.Uid), GID: uint32(sys.Gid), Mode: st.Mode()}, nil
}

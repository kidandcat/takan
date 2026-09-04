//go:build !unix

package bots

import "os"

// statOwner degrades to mode-only on platforms without unix ownership. The
// bundle import is a Linux hub-host operation, so this path only exists to keep
// the package building everywhere.
func statOwner(path string) (OwnerStat, error) {
	st, err := os.Stat(path)
	if err != nil {
		return OwnerStat{}, err
	}
	return OwnerStat{Path: path, Mode: st.Mode()}, nil
}

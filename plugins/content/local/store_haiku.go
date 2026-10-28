//go:build haiku

package local

import (
	"os"
	"syscall"
	"time"
)

func getATime(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return fi.ModTime()
}

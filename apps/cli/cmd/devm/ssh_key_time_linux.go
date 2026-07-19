//go:build linux

package main

import (
	"os"
	"syscall"
	"time"
)

func fileLastUsed(info os.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
	}
	return info.ModTime()
}

//go:build darwin

package main

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

func TestFileLastUsedUsesDarwinAccessTime(t *testing.T) {
	want := time.Date(2026, time.July, 19, 12, 34, 56, 789, time.UTC)
	info := darwinFileInfo{
		modified: want.Add(-time.Hour),
		stat:     &syscall.Stat_t{Atimespec: syscall.Timespec{Sec: want.Unix(), Nsec: int64(want.Nanosecond())}},
	}
	if got := fileLastUsed(info); !got.Equal(want) {
		t.Fatalf("last used = %s, want access time %s", got, want)
	}
}

type darwinFileInfo struct {
	modified time.Time
	stat     *syscall.Stat_t
}

func (darwinFileInfo) Name() string            { return "id_ed25519" }
func (darwinFileInfo) Size() int64             { return 0 }
func (darwinFileInfo) Mode() fs.FileMode       { return 0o600 }
func (info darwinFileInfo) ModTime() time.Time { return info.modified }
func (darwinFileInfo) IsDir() bool             { return false }
func (info darwinFileInfo) Sys() any           { return info.stat }

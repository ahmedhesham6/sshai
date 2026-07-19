//go:build !linux && !darwin

package main

import (
	"os"
	"time"
)

// Some supported filesystems do not expose a portable access timestamp.
// Modification time is the deterministic fallback on those platforms.
func fileLastUsed(info os.FileInfo) time.Time {
	return info.ModTime()
}

//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type tokenFileLock struct {
	directory *anchoredDirectory
	file      *os.File
	name      string
	info      os.FileInfo
}

func acquireTokenFileLock(ctx context.Context, directory *anchoredDirectory) (*tokenFileLock, error) {
	return acquirePrivateFileLock(ctx, directory, "tokens.lock")
}

func acquirePrivateFileLock(ctx context.Context, directory *anchoredDirectory, name string) (*tokenFileLock, error) {
	if name == "" || filepath.Base(name) != name || name == "." {
		return nil, errors.New("lock file name is invalid")
	}
	file, err := directory.root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*tokenFileLock, error) {
		_ = file.Close()
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 {
		return closeOnError(errors.New("token lock is not a private regular file"))
	}
	current, err := directory.root.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return closeOnError(errors.New("token lock changed while opening"))
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			lock := &tokenFileLock{directory: directory, file: file, name: name, info: opened}
			if !lock.StillCurrent() {
				return closeOnError(errors.New("lock file changed before acquisition completed"))
			}
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return closeOnError(err)
		}
		select {
		case <-ctx.Done():
			return closeOnError(context.Cause(ctx))
		case <-ticker.C:
		}
	}
}

// StillCurrent verifies that the pathname still names the inode on which this
// process holds flock. Callers recheck immediately before each protected
// mutation, matching the SSH include edit discipline.
func (lock *tokenFileLock) StillCurrent() bool {
	if lock == nil || lock.directory == nil || lock.file == nil || lock.info == nil {
		return false
	}
	opened, err := lock.file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, lock.info) {
		return false
	}
	current, err := lock.directory.root.Lstat(lock.name)
	return err == nil && current.Mode().IsRegular() && os.SameFile(opened, current)
}

func (lock *tokenFileLock) Close() error {
	return errors.Join(unix.Flock(int(lock.file.Fd()), unix.LOCK_UN), lock.file.Close())
}

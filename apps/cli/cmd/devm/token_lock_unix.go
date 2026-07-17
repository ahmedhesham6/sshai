//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type tokenFileLock struct{ file *os.File }

func acquireTokenFileLock(ctx context.Context, directory *anchoredDirectory) (*tokenFileLock, error) {
	const name = "tokens.lock"
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
			return &tokenFileLock{file: file}, nil
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

func (lock *tokenFileLock) Close() error {
	return errors.Join(unix.Flock(int(lock.file.Fd()), unix.LOCK_UN), lock.file.Close())
}

//go:build !linux && !darwin

package main

import (
	"context"
	"errors"
)

type tokenFileLock struct{}

func acquireTokenFileLock(context.Context, *anchoredDirectory) (*tokenFileLock, error) {
	return nil, errors.New("secure token locking is unsupported on this platform")
}

func (*tokenFileLock) Close() error { return nil }

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// anchoredDirectory keeps filesystem work relative to a verified, opened
// directory. Renames or symlink swaps of its pathname cannot redirect later
// operations.
type anchoredDirectory struct {
	root *os.Root
}

func openAnchoredDirectory(path string, create bool, mode os.FileMode) (*anchoredDirectory, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("directory path must be absolute")
	}
	volume := filepath.VolumeName(path)
	filesystemRoot := volume + string(os.PathSeparator)
	relative := strings.TrimPrefix(path, filesystemRoot)
	root, err := os.OpenRoot(filesystemRoot)
	if err != nil {
		return nil, err
	}
	parts := strings.FieldsFunc(relative, func(character rune) bool {
		return character == rune(os.PathSeparator)
	})
	for index, name := range parts {
		last := index == len(parts)-1
		info, statErr := root.Lstat(name)
		if errors.Is(statErr, os.ErrNotExist) && create {
			createMode := os.FileMode(0o700)
			if last {
				createMode = mode
			}
			if mkdirErr := root.Mkdir(name, createMode); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				root.Close()
				return nil, mkdirErr
			}
			info, statErr = root.Lstat(name)
		}
		if statErr != nil {
			root.Close()
			return nil, statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, errors.New("path contains an indirect directory")
		}
		next, openErr := root.OpenRoot(name)
		if openErr != nil {
			root.Close()
			return nil, openErr
		}
		opened, openedErr := next.Stat(".")
		current, currentErr := root.Lstat(name)
		if openedErr != nil || currentErr != nil || !current.IsDir() || !os.SameFile(opened, current) {
			next.Close()
			root.Close()
			return nil, errors.New("directory changed while opening")
		}
		root.Close()
		root = next
	}
	return &anchoredDirectory{root: root}, nil
}

func openOwnedDirectory(path string) (*anchoredDirectory, error) {
	directory, err := openAnchoredDirectory(path, true, 0o700)
	if err != nil {
		return nil, err
	}
	handle, err := directory.root.Open(".")
	if err != nil {
		directory.Close()
		return nil, err
	}
	if err := handle.Chmod(0o700); err != nil {
		handle.Close()
		directory.Close()
		return nil, err
	}
	handle.Close()
	return directory, nil
}

func (directory *anchoredDirectory) ownedChild(name string) (*anchoredDirectory, error) {
	if filepath.Base(name) != name || name == "." {
		return nil, errors.New("child directory name is invalid")
	}
	if err := directory.root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := directory.root.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("child path is not a direct directory")
	}
	root, err := directory.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	current, err := directory.root.Lstat(name)
	if err != nil || !current.IsDir() || !os.SameFile(opened, current) {
		root.Close()
		return nil, errors.New("child directory changed while opening")
	}
	child := &anchoredDirectory{root: root}
	handle, err := child.root.Open(".")
	if err != nil {
		child.Close()
		return nil, err
	}
	if err := handle.Chmod(0o700); err != nil {
		handle.Close()
		child.Close()
		return nil, err
	}
	handle.Close()
	return child, nil
}

func (directory *anchoredDirectory) Close() error {
	return directory.root.Close()
}

func (directory *anchoredDirectory) readRegular(name string, maximum int64) ([]byte, os.FileInfo, error) {
	info, err := directory.root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, nil, errors.New("path is not a bounded regular file")
	}
	file, err := directory.root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	current, err := directory.root.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return nil, nil, errors.New("file changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, nil, errors.New("read bounded regular file")
	}
	return content, opened, nil
}

func (directory *anchoredDirectory) writePrivate(name string, content []byte) error {
	if filepath.Base(name) != name || name == "." {
		return errors.New("private file name is invalid")
	}
	var temporaryName string
	var temporary *os.File
	for range 8 {
		suffix := make([]byte, 8)
		if _, err := rand.Read(suffix); err != nil {
			return err
		}
		temporaryName = ".tmp-" + hex.EncodeToString(suffix)
		var err error
		temporary, err = directory.root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if temporary == nil {
		return errors.New("allocate temporary private file")
	}
	defer directory.root.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := directory.root.Rename(temporaryName, name); err != nil {
		return err
	}
	handle, err := directory.root.Open(".")
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxUserSSHConfigSize = 1 << 20

var errLocalStateConflict = errors.New("local state changed concurrently")

// sshIncludeEdit keeps the user's primary SSH config open so Apply can detect
// path replacement and content changes before preserving that same inode.
type sshIncludeEdit struct {
	directory *anchoredDirectory
	file      *os.File
	name      string
	info      os.FileInfo
	original  []byte
	desired   []byte
}

func ensureSSHInclude(primaryConfigPath, ownedConfigPath string) error {
	for range 3 {
		edit, err := prepareSSHInclude(primaryConfigPath, ownedConfigPath)
		if err != nil {
			return err
		}
		err = edit.Apply()
		closeErr := edit.Close()
		if errors.Is(err, errLocalStateConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("update primary SSH config: %w", err)
		}
		return closeErr
	}
	return errLocalStateConflict
}

func prepareSSHInclude(primaryConfigPath, ownedConfigPath string) (*sshIncludeEdit, error) {
	includePath, err := sshConfigArgument(ownedConfigPath)
	if err != nil {
		return nil, fmt.Errorf("configure SSH include: %w", err)
	}
	directory, err := openAnchoredDirectory(filepath.Dir(primaryConfigPath), true, 0o700)
	if err != nil {
		return nil, fmt.Errorf("open primary SSH directory: %w", err)
	}
	name := filepath.Base(primaryConfigPath)
	info, err := directory.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := directory.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			directory.Close()
			return nil, createErr
		}
		info, err = file.Stat()
		if err != nil {
			file.Close()
			directory.Close()
			return nil, err
		}
		return newSSHIncludeEdit(directory, file, name, info, nil, "Include "+includePath), nil
	}
	if err != nil || !info.Mode().IsRegular() {
		directory.Close()
		return nil, errors.New("primary SSH config is not a regular file")
	}
	file, err := directory.root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		directory.Close()
		return nil, err
	}
	opened, err := file.Stat()
	current, currentErr := directory.root.Lstat(name)
	if err != nil || currentErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		file.Close()
		directory.Close()
		return nil, errors.New("primary SSH config changed while opening")
	}
	original, err := io.ReadAll(io.LimitReader(file, maxUserSSHConfigSize+1))
	if err != nil || len(original) > maxUserSSHConfigSize {
		file.Close()
		directory.Close()
		return nil, errors.New("primary SSH config exceeds size limit")
	}
	return newSSHIncludeEdit(directory, file, name, opened, original, "Include "+includePath), nil
}

func newSSHIncludeEdit(directory *anchoredDirectory, file *os.File, name string, info os.FileInfo, original []byte, include string) *sshIncludeEdit {
	withoutManaged := removeSSHInclude(original, include)
	desired := make([]byte, 0, len(include)+1+len(withoutManaged))
	desired = append(desired, include...)
	desired = append(desired, '\n')
	desired = append(desired, withoutManaged...)
	if len(withoutManaged) > 0 && desired[len(desired)-1] != '\n' {
		desired = append(desired, '\n')
	}
	return &sshIncludeEdit{
		directory: directory, file: file, name: name, info: info,
		original: append([]byte(nil), original...), desired: desired,
	}
}

func (edit *sshIncludeEdit) Apply() error {
	if !edit.stillCurrent() {
		return errLocalStateConflict
	}
	if _, err := edit.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	current, err := io.ReadAll(io.LimitReader(edit.file, maxUserSSHConfigSize+1))
	if err != nil || !bytes.Equal(current, edit.original) {
		return errLocalStateConflict
	}
	if bytes.Equal(current, edit.desired) {
		return nil
	}
	if !edit.stillCurrent() {
		return errLocalStateConflict
	}
	if err := edit.file.Truncate(0); err != nil {
		return err
	}
	if _, err := edit.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := edit.file.Write(edit.desired); err != nil {
		return err
	}
	return edit.file.Sync()
}

func (edit *sshIncludeEdit) stillCurrent() bool {
	current, err := edit.directory.root.Lstat(edit.name)
	return err == nil && current.Mode().IsRegular() && os.SameFile(edit.info, current)
}

func (edit *sshIncludeEdit) Close() error {
	return errors.Join(edit.file.Close(), edit.directory.Close())
}

func removeSSHInclude(content []byte, include string) []byte {
	lines := strings.SplitAfter(string(content), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != include {
			kept = append(kept, line)
		}
	}
	return []byte(strings.Join(kept, ""))
}

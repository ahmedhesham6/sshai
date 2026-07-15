//go:build linux

package guest

import "golang.org/x/sys/unix"

func publishProjectSeedWorkspace(staging, workspace string) error {
	return unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, workspace, unix.RENAME_NOREPLACE)
}

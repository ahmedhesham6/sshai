//go:build !linux

package guest

import "errors"

func publishProjectSeedWorkspace(_, _ string) error {
	return errors.New("atomic Project Seed publication requires Linux")
}

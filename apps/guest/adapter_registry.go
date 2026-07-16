package guest

import (
	"fmt"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

type capsuleComponentAdapter interface {
	ID() string
	Version() string
	Translate(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []capsuleFile, installed InstalledMaterialization, hasInstalled bool, batch CapsuleLockMaterializationBatch) (ProfileMaterialization, error)
}

var capsuleComponentAdapters = map[string]capsuleComponentAdapter{}

func registerCapsuleAdapter(a capsuleComponentAdapter) {
	capsuleComponentAdapters[a.ID()] = a
}

func capsuleAdapterFor(id string) (capsuleComponentAdapter, error) {
	adapter, ok := capsuleComponentAdapters[id]
	if !ok {
		return nil, fmt.Errorf("unknown capsule component adapter %q", id)
	}
	return adapter, nil
}

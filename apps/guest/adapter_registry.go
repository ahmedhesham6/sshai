package guest

import (
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

type sensitiveMaterializationSurface struct {
	Target   string
	Selector string
	Reason   string
}

func sensitiveMaterializationApproval(target, selector string, surfaces []sensitiveMaterializationSurface) (bool, string) {
	if selector == "" {
		selector = "$"
	}
	reasons := make([]string, 0, len(surfaces))
	for _, surface := range surfaces {
		if target != surface.Target {
			continue
		}
		surfaceSelector := surface.Selector
		if surfaceSelector == "" {
			surfaceSelector = "$"
		}
		if !materializationSelectorsOverlap(selector, surfaceSelector) {
			continue
		}
		duplicate := false
		for _, existing := range reasons {
			if existing == surface.Reason {
				duplicate = true
				break
			}
		}
		if !duplicate && surface.Reason != "" {
			reasons = append(reasons, surface.Reason)
		}
	}
	return len(reasons) > 0, strings.Join(reasons, "; ")
}

func materializationSelectorsOverlap(left, right string) bool {
	return left == right || left == "$" || right == "$" || strings.HasPrefix(left, right+".") || strings.HasPrefix(right, left+".")
}

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

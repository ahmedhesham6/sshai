// Package adapters holds the per-agent compiler backends translating
// canonical Components into native change plans (spec/16).
package adapters

import (
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
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
		if !MaterializationSelectorsOverlap(selector, surfaceSelector) {
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

func MaterializationSelectorsOverlap(left, right string) bool {
	return left == right || left == "$" || right == "$" || strings.HasPrefix(left, right+".") || strings.HasPrefix(right, left+".")
}

type Adapter interface {
	ID() string
	Version() string
	Translate(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []profile.CapsuleFile, installed profile.InstalledMaterialization, hasInstalled bool, batch profile.CapsuleLockMaterializationBatch) (profile.ProfileMaterialization, error)
}

var capsuleComponentAdapters = map[string]Adapter{}

func Register(a Adapter) {
	capsuleComponentAdapters[a.ID()] = a
}

func For(id string) (Adapter, error) {
	adapter, ok := capsuleComponentAdapters[id]
	if !ok {
		return nil, fmt.Errorf("unknown capsule component adapter %q", id)
	}
	return adapter, nil
}

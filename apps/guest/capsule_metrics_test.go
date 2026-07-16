package guest_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/capsule"
)

func TestCapsuleMaterializationEmitsPullAndComponentMetricsAtTheirSeams(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"CLAUDE.md": "instructions\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	metrics := &guestCounterMetricsFake{counts: make(map[string]int64)}
	request := lockMaterializationRequest(t, provider, value)
	request.Metrics = metrics
	first, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err != nil {
		t.Fatalf("cold materialization: %v", err)
	}
	if metrics.counts["capsule_pulls_total"] != 1 || metrics.counts["component_materializations_total"] != 1 || metrics.counts["component_conflicts_total"] != 0 {
		t.Fatalf("cold materialization metrics = %#v, want one pull/materialization and no conflicts", metrics.counts)
	}
	request.Installed = guest.InstalledMaterializationsFromResults(first)
	if _, err := guest.MaterializeCapsuleLock(t.Context(), request); err != nil {
		t.Fatalf("warm materialization: %v", err)
	}
	if metrics.counts["capsule_pulls_total"] != 1 || metrics.counts["component_materializations_total"] != 1 {
		t.Fatalf("warm materialization metrics = %#v, want no new pull or skip materialization", metrics.counts)
	}
}

type guestCounterMetricsFake struct {
	counts map[string]int64
}

func (metrics *guestCounterMetricsFake) AddCounter(name string, value int64) {
	metrics.counts[name] += value
}

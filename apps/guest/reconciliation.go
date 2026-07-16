package guest

import "github.com/ahmedhesham6/sshai/libs/domain"

// ReconcileCapsuleLockMaterializations adapts durable guest materialization
// state to the pure domain lock comparison.
func ReconcileCapsuleLockMaterializations(lock domain.CapsuleLockSnapshot, installed []InstalledMaterialization) []domain.ComponentDivergence {
	states := make([]domain.InstalledComponentState, 0, len(installed))
	for _, materialization := range installed {
		states = append(states, domain.InstalledComponentState{
			ComponentID: materialization.ComponentID, LockDigest: materialization.LockDigest,
			CapsuleDigest: materialization.CapsuleDigest, ComponentDigest: materialization.ComponentDigest,
		})
	}
	return domain.ReconcileCapsuleLockComponents(lock, states)
}

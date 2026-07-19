package domain

import "time"

// RuntimeStopAuditEvidence preserves the Auto-stop decision inputs that caused
// a runtime.stop Operation. Manual and operator-initiated stops leave it nil.
type RuntimeStopAuditEvidence struct {
	Policy              AutoStopPolicySnapshot     `json:"policy"`
	PolicyGeneration    uint64                     `json:"policyGeneration"`
	QualifyingSnapshots []AutoStopActivitySnapshot `json:"qualifyingSnapshots"`
	GraceStartedAt      time.Time                  `json:"graceStartedAt"`
	GraceExpiredAt      time.Time                  `json:"graceExpiredAt"`
	GracePeriodSeconds  int                        `json:"gracePeriodSeconds"`
}

func CloneRuntimeStopAuditEvidence(evidence *RuntimeStopAuditEvidence) *RuntimeStopAuditEvidence {
	if evidence == nil {
		return nil
	}
	clone := *evidence
	clone.QualifyingSnapshots = append([]AutoStopActivitySnapshot(nil), evidence.QualifyingSnapshots...)
	return &clone
}

package domain

type RuntimeOperationDispatch struct {
	OperationID   string
	OperationType OperationType
	EnvironmentID string
	RuntimeID     string
	OwnerUserID   string
	StopReason    RuntimeStopReason
	StopAudit     *RuntimeStopAuditEvidence
}

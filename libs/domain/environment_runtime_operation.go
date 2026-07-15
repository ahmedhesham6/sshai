package domain

import "errors"

type EnvironmentRuntimeOperation struct {
	environment Environment
	runtime     Runtime
	operation   Operation
}

func NewEnvironmentRuntimeOperation(environment Environment, runtime Runtime, operation Operation) (EnvironmentRuntimeOperation, error) {
	environmentSnapshot := environment.Snapshot()
	runtimeSnapshot := runtime.Snapshot()
	operationSnapshot := operation.Snapshot()
	if environmentSnapshot.Lifecycle != EnvironmentActive {
		return EnvironmentRuntimeOperation{}, errors.New("create Runtime Operation: Environment is not active")
	}
	if err := validateRuntimeOwnership(environmentSnapshot, runtimeSnapshot); err != nil {
		return EnvironmentRuntimeOperation{}, errors.New("create Runtime Operation: Runtime does not belong to Environment")
	}
	if !operationSnapshot.Status.terminal() && (environmentSnapshot.CurrentRuntimeID == nil || *environmentSnapshot.CurrentRuntimeID != runtimeSnapshot.ID) {
		return EnvironmentRuntimeOperation{}, errors.New("create Runtime Operation: Runtime is not current for Environment")
	}
	if operationSnapshot.EnvironmentID != environmentSnapshot.ID || operationSnapshot.RequestedByUserID != environmentSnapshot.OwnerUserID {
		return EnvironmentRuntimeOperation{}, errors.New("create Runtime Operation: Operation does not belong to Environment owner")
	}
	if !operationSnapshot.Type.runtimeCommand() {
		return EnvironmentRuntimeOperation{}, errors.New("create Runtime Operation: Operation type is not a Runtime command")
	}
	if !operationSnapshot.Status.terminal() && !operationSnapshot.Type.acceptsRuntimeState(runtimeSnapshot.Status, operationSnapshot.Status) {
		return EnvironmentRuntimeOperation{}, errors.New("create Runtime Operation: current Runtime status does not accept command")
	}
	return EnvironmentRuntimeOperation{environment: environment, runtime: runtime, operation: operation}, nil
}

func (command EnvironmentRuntimeOperation) Environment() Environment { return command.environment }
func (command EnvironmentRuntimeOperation) Runtime() Runtime         { return command.runtime }
func (command EnvironmentRuntimeOperation) Operation() Operation     { return command.operation }

func (operationType OperationType) runtimeCommand() bool {
	return operationType == OperationRuntimeStart || operationType == OperationRuntimeStop || operationType == OperationRuntimeReplace
}

func (operationType OperationType) acceptsRuntimeState(runtimeStatus RuntimeStatus, operationStatus OperationStatus) bool {
	switch operationType {
	case OperationRuntimeStart:
		return runtimeStatus == RuntimeReady || runtimeStatus == RuntimeStopped || runtimeStatus == RuntimeStarting && operationStatus == OperationRunning
	case OperationRuntimeStop:
		return runtimeStatus == RuntimeReady || runtimeStatus == RuntimeStopped || runtimeStatus == RuntimeStopping && operationStatus == OperationRunning
	case OperationRuntimeReplace:
		return runtimeStatus == RuntimeReady || runtimeStatus == RuntimeStopped || runtimeStatus == RuntimeError || runtimeStatus == RuntimeReplacing && operationStatus == OperationRunning
	default:
		return false
	}
}

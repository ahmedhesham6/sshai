package domain

type RuntimeStopReason string

const (
	RuntimeStopManual   RuntimeStopReason = "manual"
	RuntimeStopAutoStop RuntimeStopReason = "auto_stop"
	RuntimeStopBilling  RuntimeStopReason = "billing"
	RuntimeStopRepair   RuntimeStopReason = "repair"
	RuntimeStopResize   RuntimeStopReason = "resize"
)

func (reason RuntimeStopReason) Valid() bool {
	switch reason {
	case RuntimeStopManual, RuntimeStopAutoStop, RuntimeStopBilling, RuntimeStopRepair, RuntimeStopResize:
		return true
	default:
		return false
	}
}

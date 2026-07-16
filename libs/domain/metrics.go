package domain

const (
	MetricComponentConflictsTotal        = "component_conflicts_total"
	MetricLockResolutionsTotal           = "lock_resolutions_total"
	MetricComponentMaterializationsTotal = "component_materializations_total"
	MetricCapsulePullsTotal              = "capsule_pulls_total"
)

// Metrics is the narrow package seam used by workflows and guest code. An
// adapter can forward these low-cardinality names to OpenTelemetry.
type Metrics interface {
	AddCounter(name string, value int64)
}

type MetricsFunc func(name string, value int64)

func (fn MetricsFunc) AddCounter(name string, value int64) {
	if fn != nil {
		fn(name, value)
	}
}

package main

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
)

func TestBuildServicesWithBillingOnlyDependencies(t *testing.T) {
	services := buildServices(serviceDependencies{now: time.Now})
	names := make([]string, len(services))
	for index, service := range services {
		names[index] = service.Name()
	}
	if len(names) != 1 || names[0] != workflows.BillingDeliveryService {
		t.Fatalf("bound services = %v, want [%s]", names, workflows.BillingDeliveryService)
	}
}

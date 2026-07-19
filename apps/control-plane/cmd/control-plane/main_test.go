package main

import "testing"

func TestLoadConfigUsesRatifiedRegionDefaults(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL": "postgres://example", "WORKOS_CLIENT_ID": "client-1",
		"UPLOAD_BUCKET": "uploads", "CAPSULE_BUCKET": "capsules",
	} {
		t.Setenv(name, value)
	}
	t.Setenv("DEFAULT_REGION", "")
	t.Setenv("DEFAULT_AVAILABILITY_ZONE", "")

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig(): %v", err)
	}
	if config.defaultRegion != "eu-central-1" || config.defaultAvailabilityZone != "eu-central-1a" {
		t.Fatalf("region defaults = %q/%q", config.defaultRegion, config.defaultAvailabilityZone)
	}
}

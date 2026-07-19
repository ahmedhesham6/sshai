package main

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesRatifiedRegionDefaults(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL": "postgres://example", "WORKOS_CLIENT_ID": "client-1",
		"UPLOAD_BUCKET": "uploads", "CAPSULE_BUCKET": "capsules",
		"REGIONAL_PROXY_URLS": `{"eu-central-1":"wss://proxy.example.test"}`,
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
	if config.connectionIntentTTL != 60*time.Second || config.regionalProxyURLs["eu-central-1"] != "wss://proxy.example.test" {
		t.Fatalf("connection config = %s/%#v", config.connectionIntentTTL, config.regionalProxyURLs)
	}
}

func TestLoadConfigRejectsMissingEnabledRegionalProxy(t *testing.T) {
	for _, regionalURLs := range []string{`{}`, `null`, `{"eu-west-1":"wss://proxy.example.test"}`} {
		t.Run(regionalURLs, func(t *testing.T) {
			for name, value := range map[string]string{
				"DATABASE_URL": "postgres://example", "WORKOS_CLIENT_ID": "client-1",
				"UPLOAD_BUCKET": "uploads", "CAPSULE_BUCKET": "capsules",
				"REGIONAL_PROXY_URLS": regionalURLs,
			} {
				t.Setenv(name, value)
			}
			t.Setenv("DEFAULT_REGION", "eu-central-1")

			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), `REGIONAL_PROXY_URLS must contain enabled DEFAULT_REGION "eu-central-1"`) {
				t.Fatalf("loadConfig() error = %v", err)
			}
		})
	}
}

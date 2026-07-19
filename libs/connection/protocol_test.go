package connection_test

import (
	"net/url"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/connection"
)

func TestProxyURLContractIsSharedByBuilderAndValidator(t *testing.T) {
	base, err := url.Parse("wss://proxy.example.test/configured/base")
	if err != nil {
		t.Fatal(err)
	}
	built := connection.ProxyURL(base, "env_01")
	parsed, err := connection.ValidateProxyURL(built, "env_01")
	if err != nil || parsed.String() != "wss://proxy.example.test/v1/environments/env_01/ssh" {
		t.Fatalf("proxy URL = %q parsed:%v error:%v", built, parsed, err)
	}
	for _, unsafe := range []string{
		"ws://proxy.example.test/v1/environments/env_01/ssh",
		"wss://user@proxy.example.test/v1/environments/env_01/ssh",
		"wss://proxy.example.test/v1/environments/env_01/ssh?token=secret",
		"wss://proxy.example.test/v1/environments/other/ssh",
	} {
		if _, err := connection.ValidateProxyURL(unsafe, "env_01"); err == nil {
			t.Fatalf("accepted unsafe proxy URL %q", unsafe)
		}
	}
}

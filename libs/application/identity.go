package application

import "strings"

func canonicalIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

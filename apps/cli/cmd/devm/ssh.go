package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
)

const maxPublicKeySize = 16 << 10

type localSSHKey struct {
	Label          string
	PrivateKeyPath string
	PublicKey      string
	Fingerprint    string
	LastUsed       time.Time
}

// discoverEd25519Keys reads public-key files only. A regular sibling private
// key path is required for OpenSSH configuration, but its contents are never
// opened by discovery.
func discoverEd25519Keys(sshDirectory string) ([]localSSHKey, error) {
	directory, err := openAnchoredDirectory(sshDirectory, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open SSH directory: %w", err)
	}
	defer directory.Close()
	handle, err := directory.root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("read SSH directory: %w", err)
	}
	entries, err := handle.ReadDir(-1)
	handle.Close()
	if err != nil {
		return nil, fmt.Errorf("read SSH directory: %w", err)
	}

	keys := make([]localSSHKey, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pub") {
			continue
		}
		publicContent, _, err := directory.readRegular(entry.Name(), maxPublicKeySize)
		if err != nil {
			continue
		}
		privateName := strings.TrimSuffix(entry.Name(), ".pub")
		privateInfo, err := directory.root.Lstat(privateName)
		if err != nil || !privateInfo.Mode().IsRegular() {
			continue
		}
		publicKey, fingerprint, ok := parseEd25519PublicKey(publicContent)
		if !ok {
			continue
		}
		keys = append(keys, localSSHKey{
			Label:          privateName,
			PrivateKeyPath: filepath.Join(sshDirectory, privateName),
			PublicKey:      publicKey,
			Fingerprint:    fingerprint,
			LastUsed:       fileLastUsed(privateInfo),
		})
	}
	sort.Slice(keys, func(left, right int) bool {
		return keys[left].PrivateKeyPath < keys[right].PrivateKeyPath
	})
	return keys, nil
}

func parseEd25519PublicKey(content []byte) (string, string, bool) {
	publicKey, _, options, rest, err := ssh.ParseAuthorizedKey(content)
	if err != nil || publicKey.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return "", "", false
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))
	return canonical, ssh.FingerprintSHA256(publicKey), true
}

type sshEnvironmentConfig struct {
	Alias         string
	EnvironmentID string
	IdentityFile  string
}

var sshIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func renderSSHConfig(entries []sshEnvironmentConfig, knownHostsPath string) (string, error) {
	knownHosts, err := sshLiteralPathArgument(knownHostsPath)
	if err != nil {
		return "", fmt.Errorf("render SSH config: known-hosts path: %w", err)
	}
	ordered := append([]sshEnvironmentConfig(nil), entries...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Alias < ordered[right].Alias })
	seen := make(map[string]struct{}, len(ordered))
	var config strings.Builder
	for index, entry := range ordered {
		if !sshIdentifierPattern.MatchString(entry.Alias) {
			return "", fmt.Errorf("render SSH config: invalid alias %q", entry.Alias)
		}
		if !sshIdentifierPattern.MatchString(entry.EnvironmentID) {
			return "", fmt.Errorf("render SSH config: invalid Environment ID %q", entry.EnvironmentID)
		}
		if _, exists := seen[entry.Alias]; exists {
			return "", fmt.Errorf("render SSH config: duplicate alias %q", entry.Alias)
		}
		seen[entry.Alias] = struct{}{}
		identityFile, err := sshLiteralPathArgument(entry.IdentityFile)
		if err != nil {
			return "", fmt.Errorf("render SSH config: identity file for %q: %w", entry.Alias, err)
		}
		if index > 0 {
			config.WriteByte('\n')
		}
		fmt.Fprintf(&config, "Host %s\n", entry.Alias)
		fmt.Fprintf(&config, "    HostName %s\n", entry.EnvironmentID)
		config.WriteString("    User dev\n")
		fmt.Fprintf(&config, "    IdentityFile %s\n", identityFile)
		config.WriteString("    IdentitiesOnly yes\n")
		fmt.Fprintf(&config, "    UserKnownHostsFile %s\n", knownHosts)
		config.WriteString("    ProxyCommand devm ssh-proxy --environment %h\n")
		config.WriteString("    ServerAliveInterval 30\n")
	}
	return config.String(), nil
}

func sshConfigArgument(value string) (string, error) {
	if value == "" {
		return "", errors.New("value is required")
	}
	for _, character := range value {
		if character == '\r' || character == '\n' || character == 0 || unicode.IsControl(character) {
			return "", errors.New("control characters are forbidden")
		}
	}
	if strings.IndexFunc(value, unicode.IsSpace) == -1 && !strings.ContainsAny(value, `"\\`) {
		return value, nil
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`, nil
}

func sshLiteralPathArgument(value string) (string, error) {
	return sshConfigArgument(strings.ReplaceAll(value, "%", "%%"))
}

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestDiscoverEd25519KeysReturnsOnlyUsablePublicMetadata(t *testing.T) {
	sshDirectory := t.TempDir()
	publicLine, fingerprint := writeEd25519KeyPair(t, sshDirectory, "id_work", "work laptop")
	if err := os.WriteFile(filepath.Join(sshDirectory, "id_rsa.pub"), []byte("ssh-rsa invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "notes.txt"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := discoverEd25519Keys(sshDirectory)
	if err != nil {
		t.Fatalf("discoverEd25519Keys(): %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %#v", keys)
	}
	key := keys[0]
	if key.Label != "id_work" || key.PrivateKeyPath != filepath.Join(sshDirectory, "id_work") {
		t.Fatalf("key metadata = %#v", key)
	}
	if key.PublicKey != publicLine || key.Fingerprint != fingerprint {
		t.Fatalf("public key metadata = %#v", key)
	}
	if strings.Contains(key.PublicKey, "work laptop") {
		t.Fatal("discovery retained the local public-key comment")
	}
}

func TestDiscoverEd25519KeysRejectsUnsafeOrAmbiguousCandidates(t *testing.T) {
	sshDirectory := t.TempDir()
	writeEd25519KeyPair(t, sshDirectory, "id_valid", "")
	publicLine, _ := newEd25519PublicLine(t, "")

	if err := os.WriteFile(filepath.Join(sshDirectory, "missing_private.pub"), []byte(publicLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private"), []byte("do not read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "public"), []byte(publicLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "public"), filepath.Join(sshDirectory, "public_link.pub")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "private_link.pub"), []byte(publicLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "private"), filepath.Join(sshDirectory, "private_link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "multiple.pub"), []byte(publicLine+"\n"+publicLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "multiple"), []byte("do not read"), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := discoverEd25519Keys(sshDirectory)
	if err != nil {
		t.Fatalf("discoverEd25519Keys(): %v", err)
	}
	if len(keys) != 1 || keys[0].Label != "id_valid" {
		t.Fatalf("unsafe candidates were returned: %#v", keys)
	}
}

func TestDiscoverEd25519KeysDoesNotReadPrivateKeyContents(t *testing.T) {
	sshDirectory := t.TempDir()
	publicLine, fingerprint := newEd25519PublicLine(t, "")
	if err := os.WriteFile(filepath.Join(sshDirectory, "id_agent.pub"), []byte(publicLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDirectory, "id_agent"), nil, 0); err != nil {
		t.Fatal(err)
	}

	keys, err := discoverEd25519Keys(sshDirectory)
	if err != nil {
		t.Fatalf("discoverEd25519Keys(): %v", err)
	}
	if len(keys) != 1 || keys[0].PublicKey != publicLine || keys[0].Fingerprint != fingerprint {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestDiscoverEd25519KeysRejectsSymlinkedScannerRoot(t *testing.T) {
	outside := t.TempDir()
	writeEd25519KeyPair(t, outside, "id_outside", "")
	root := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverEd25519Keys(root); err == nil {
		t.Fatal("discoverEd25519Keys() accepted a symlinked scanner root")
	}
}

func TestChooseLocalSSHKeyFollowsRatifiedRules(t *testing.T) {
	sshDirectory := t.TempDir()
	writeEd25519KeyPair(t, sshDirectory, "id_older", "")
	writeEd25519KeyPair(t, sshDirectory, "id_recent", "")
	now := time.Now().Truncate(time.Second)
	older := now.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(sshDirectory, "id_older"), older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(sshDirectory, "id_recent"), now, now); err != nil {
		t.Fatal(err)
	}
	keys, err := discoverEd25519Keys(sshDirectory)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		keys       []localSSHKey
		override   string
		wantLabel  string
		wantNotice bool
	}{
		{name: "none requests generation", keys: nil},
		{name: "single is silent", keys: keys[:1], wantLabel: "id_older"},
		{name: "multiple picks most recently used", keys: keys, wantLabel: "id_recent", wantNotice: true},
		{name: "override by path", keys: keys, override: filepath.Join(sshDirectory, "id_older"), wantLabel: "id_older"},
		{name: "override by label", keys: keys, override: "id_older", wantLabel: "id_older"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, notice, err := chooseLocalSSHKey(test.keys, test.override)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Label != test.wantLabel || notice != test.wantNotice {
				t.Fatalf("selection = label:%q notice:%t", selected.Label, notice)
			}
		})
	}
	if _, _, err := chooseLocalSSHKey(keys, filepath.Join(sshDirectory, "missing")); err == nil {
		t.Fatal("missing override was accepted")
	}
}

func TestRenderSSHConfigIsDeterministicAndUsesStableLogicalHost(t *testing.T) {
	entries := []sshEnvironmentConfig{
		{Alias: "zeta", EnvironmentID: "env_02", IdentityFile: "/home/alice/.ssh/id_zeta"},
		{Alias: "api-dev", EnvironmentID: "env_01", IdentityFile: "/home/alice/keys/dev key"},
	}
	got, err := renderSSHConfig(entries, "/home/alice/.config/devm/known_hosts")
	if err != nil {
		t.Fatalf("renderSSHConfig(): %v", err)
	}
	want := "Host api-dev\n" +
		"    HostName env_01\n" +
		"    User dev\n" +
		"    IdentityFile \"/home/alice/keys/dev key\"\n" +
		"    IdentitiesOnly yes\n" +
		"    UserKnownHostsFile /home/alice/.config/devm/known_hosts\n" +
		"    ProxyCommand devm ssh-proxy --environment %h\n" +
		"    ServerAliveInterval 30\n\n" +
		"Host zeta\n" +
		"    HostName env_02\n" +
		"    User dev\n" +
		"    IdentityFile /home/alice/.ssh/id_zeta\n" +
		"    IdentitiesOnly yes\n" +
		"    UserKnownHostsFile /home/alice/.config/devm/known_hosts\n" +
		"    ProxyCommand devm ssh-proxy --environment %h\n" +
		"    ServerAliveInterval 30\n"
	if got != want {
		t.Fatalf("config:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderSSHConfigRejectsInjectionAndDuplicateAliases(t *testing.T) {
	tests := []struct {
		name    string
		entries []sshEnvironmentConfig
		known   string
	}{
		{name: "alias injection", entries: []sshEnvironmentConfig{{Alias: "safe\nHost *", EnvironmentID: "env_01", IdentityFile: "/key"}}, known: "/known"},
		{name: "environment injection", entries: []sshEnvironmentConfig{{Alias: "safe", EnvironmentID: "env_01 ProxyCommand bad", IdentityFile: "/key"}}, known: "/known"},
		{name: "identity newline", entries: []sshEnvironmentConfig{{Alias: "safe", EnvironmentID: "env_01", IdentityFile: "/key\nProxyCommand bad"}}, known: "/known"},
		{name: "known hosts newline", entries: []sshEnvironmentConfig{{Alias: "safe", EnvironmentID: "env_01", IdentityFile: "/key"}}, known: "/known\nbad"},
		{name: "duplicate alias", entries: []sshEnvironmentConfig{{Alias: "safe", EnvironmentID: "env_01", IdentityFile: "/key"}, {Alias: "safe", EnvironmentID: "env_02", IdentityFile: "/key"}}, known: "/known"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := renderSSHConfig(test.entries, test.known); err == nil {
				t.Fatal("renderSSHConfig() error = nil")
			}
		})
	}
}

func TestRenderSSHConfigEscapesLiteralOpenSSHPercentTokensInPaths(t *testing.T) {
	config, err := renderSSHConfig([]sshEnvironmentConfig{{
		Alias: "api-dev", EnvironmentID: "env_01", IdentityFile: "/home/alice/keys/id_%h_%b",
	}}, "/home/alice/.config/devm/%h/known_hosts")
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{"IdentityFile /home/alice/keys/id_%%h_%%b", "UserKnownHostsFile /home/alice/.config/devm/%%h/known_hosts"} {
		if !strings.Contains(config, literal) {
			t.Fatalf("config did not escape %q:\n%s", literal, config)
		}
	}
}

func TestEnsureSSHIncludeAddsExactlyOneOwnedInclude(t *testing.T) {
	directory := t.TempDir()
	primary := filepath.Join(directory, ".ssh", "config")
	owned := filepath.Join(directory, ".config", "devm", "ssh", "config")
	if err := os.MkdirAll(filepath.Dir(primary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primary, []byte("Host existing\n    User alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ensureSSHInclude(primary, owned); err != nil {
		t.Fatalf("ensureSSHInclude(): %v", err)
	}
	if err := ensureSSHInclude(primary, owned); err != nil {
		t.Fatalf("ensureSSHInclude() replay: %v", err)
	}
	content, err := os.ReadFile(primary)
	if err != nil {
		t.Fatal(err)
	}
	include := "Include " + owned
	if strings.Count(string(content), include) != 1 {
		t.Fatalf("config = %q", content)
	}
	if !strings.HasPrefix(string(content), include+"\n") {
		t.Fatalf("include was not placed before user Host stanzas: %q", content)
	}
	assertMode(t, primary, 0o600)
}

func TestEnsureSSHIncludeMakesOwnedEnvironmentSettingsWinOpenSSHResolution(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is unavailable")
	}
	directory := t.TempDir()
	primary := filepath.Join(directory, ".ssh", "config")
	owned := filepath.Join(directory, ".config", "devm", "ssh", "config")
	if err := os.MkdirAll(filepath.Dir(primary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(owned), 0o700); err != nil {
		t.Fatal(err)
	}
	conflicting := "Host *\n    ProxyCommand wrong-command\n    HostName wrong-host\n    User wrong-user\n    IdentitiesOnly no\n"
	if err := os.WriteFile(primary, []byte(conflicting), 0o600); err != nil {
		t.Fatal(err)
	}
	generated, err := renderSSHConfig([]sshEnvironmentConfig{{
		Alias: "api-dev", EnvironmentID: "env_01", IdentityFile: "/home/alice/.ssh/id_%h",
	}}, filepath.Join(directory, ".config", "devm", "%b", "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owned, []byte(generated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSSHInclude(primary, owned); err != nil {
		t.Fatal(err)
	}

	output, err := exec.Command("ssh", "-G", "-F", primary, "api-dev").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G: %v\n%s", err, output)
	}
	effective := string(output)
	for _, setting := range []string{
		"hostname env_01\n",
		"user dev\n",
		"proxycommand devm ssh-proxy --environment %h\n",
		"identitiesonly yes\n",
		"identityfile /home/alice/.ssh/id_%%h\n",
		"userknownhostsfile " + filepath.Join(directory, ".config", "devm", "%b", "known_hosts") + "\n",
	} {
		if !strings.Contains(effective, setting) {
			t.Fatalf("effective config lacks %q:\n%s", setting, effective)
		}
	}
	for _, unsafe := range []string{"wrong-command", "hostname wrong-host", "user wrong-user"} {
		if strings.Contains(effective, unsafe) {
			t.Fatalf("effective config retained %q:\n%s", unsafe, effective)
		}
	}
}

func TestEnsureSSHIncludeRejectsSymlinkedPrimaryConfig(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(directory, "config")
	if err := os.Symlink(outside, primary); err != nil {
		t.Fatal(err)
	}
	if err := ensureSSHInclude(primary, filepath.Join(directory, "owned")); err == nil {
		t.Fatal("ensureSSHInclude() accepted a symlink")
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "sentinel" {
		t.Fatal("symlink target was modified")
	}
}

func TestEnsureSSHIncludeNormalizesStaleManagedDuplicates(t *testing.T) {
	directory := t.TempDir()
	primary := filepath.Join(directory, ".ssh", "config")
	owned := filepath.Join(directory, ".config", "devm", "ssh", "config")
	if err := os.MkdirAll(filepath.Dir(primary), 0o700); err != nil {
		t.Fatal(err)
	}
	include := "Include " + owned
	content := include + "\nHost existing\n    User alice\n" + include + "\n"
	if err := os.WriteFile(primary, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(primary, 0o640); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "config-hardlink")
	if err := os.Link(primary, alias); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(primary)
	if err != nil {
		t.Fatal(err)
	}

	if err := ensureSSHInclude(primary, owned); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(primary)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("primary SSH config inode was replaced")
	}
	if after.Mode().Perm() != 0o640 {
		t.Fatalf("primary SSH config mode = %o", after.Mode().Perm())
	}
	updated, err := os.ReadFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(updated), include) != 1 || !strings.HasPrefix(string(updated), include+"\n") {
		t.Fatalf("managed include was not normalized through the original inode: %q", updated)
	}
}

func TestSSHIncludeEditDetectsConcurrentUserChangeAndRetryPreservesIt(t *testing.T) {
	directory := t.TempDir()
	primary := filepath.Join(directory, ".ssh", "config")
	owned := filepath.Join(directory, ".config", "devm", "ssh", "config")
	if err := os.MkdirAll(filepath.Dir(primary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primary, []byte("Host original\n    User alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edit, err := prepareSSHInclude(primary, owned)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primary, []byte("# concurrent edit\nHost changed\n    User bob\n"), 0o600); err != nil {
		edit.Close()
		t.Fatal(err)
	}
	if err := edit.Apply(); !errors.Is(err, errLocalStateConflict) {
		edit.Close()
		t.Fatalf("Apply() error = %v", err)
	}
	if err := edit.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ensureSSHInclude(primary, owned); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(primary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "# concurrent edit\nHost changed\n    User bob\n") {
		t.Fatalf("concurrent edit was lost: %q", content)
	}
}

func writeEd25519KeyPair(t *testing.T, directory, name, comment string) (string, string) {
	t.Helper()
	publicLine, fingerprint := newEd25519PublicLine(t, comment)
	if err := os.WriteFile(filepath.Join(directory, name+".pub"), []byte(publicLine+" "+comment+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte("private key contents must remain unread"), 0o600); err != nil {
		t.Fatal(err)
	}
	return publicLine, fingerprint
}

func newEd25519PublicLine(t *testing.T, _ string) (string, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), ssh.FingerprintSHA256(key)
}

package capsule

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestManifestCanonicalJSON(t *testing.T) {
	tests := []struct {
		name     string
		manifest Manifest
		want     string
	}{
		{
			name: "stable field and component order",
			manifest: Manifest{
				SchemaVersion: 1,
				Name:          "developer-defaults",
				Components: []Component{
					{
						ID:         "skill:fix-ci",
						Type:       ComponentTypeSkill,
						Scope:      ScopeUser,
						TrustClass: TrustExecutable,
						MediaType:  LayerMediaType(ComponentTypeSkill),
						Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
						SizeBytes:  42,
						Requirements: Requirements{
							Commands: []string{"go", "git"},
							Secrets:  []string{"GITHUB_TOKEN"},
						},
					},
				},
				Requirements: Requirements{
					Commands: []string{"git", "go"},
					Secrets:  []string{"SSH_AUTH_SOCK", "GITHUB_TOKEN"},
				},
			},
			want: `{"schemaVersion":1,"name":"developer-defaults","components":[{"id":"skill:fix-ci","type":"skill","scope":"user","trustClass":"executable","mediaType":"application/vnd.devm.capsule.skill.v1.tar+gzip","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","sizeBytes":42,"requirements":{"commands":["git","go"],"secrets":["GITHUB_TOKEN"]}}],"requirements":{"commands":["git","go"],"secrets":["GITHUB_TOKEN","SSH_AUTH_SOCK"]}}`,
		},
		{
			name: "empty requirements remain explicit arrays",
			manifest: Manifest{
				SchemaVersion: 1,
				Name:          "minimal",
				Components: []Component{
					{
						ID:         "config:editor",
						Type:       ComponentTypeConfig,
						Scope:      ScopeProject,
						TrustClass: TrustDeclarative,
						MediaType:  LayerMediaType(ComponentTypeConfig),
						Digest:     "sha256:2222222222222222222222222222222222222222222222222222222222222222",
						SizeBytes:  0,
					},
				},
			},
			want: `{"schemaVersion":1,"name":"minimal","components":[{"id":"config:editor","type":"config","scope":"project","trustClass":"declarative","mediaType":"application/vnd.devm.capsule.config.v1.tar+gzip","digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","sizeBytes":0,"requirements":{"commands":[],"secrets":[]}}],"requirements":{"commands":[],"secrets":[]}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.manifest.CanonicalJSON()
			if err != nil {
				t.Fatalf("CanonicalJSON() error = %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("CanonicalJSON() = %s, want %s", got, test.want)
			}
			if !json.Valid(got) {
				t.Fatal("CanonicalJSON() returned invalid JSON")
			}
		})
	}
}

func TestManifestCanonicalJSONDeduplicatesRequirements(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "deduplicated",
		Components: []Component{
			{
				ID:         "config:editor",
				Type:       ComponentTypeConfig,
				Scope:      ScopeUser,
				TrustClass: TrustDeclarative,
				Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Requirements: Requirements{
					Commands: []string{"git", "go", "git"},
					Secrets:  []string{"SSH_AUTH_SOCK", "SSH_AUTH_SOCK"},
				},
			},
		},
		Requirements: Requirements{
			Commands: []string{"go", "git", "go"},
			Secrets:  []string{"TOKEN", "TOKEN"},
		},
	}
	withoutDuplicates := manifest
	withoutDuplicates.Components = append([]Component(nil), manifest.Components...)
	withoutDuplicates.Components[0].Requirements = Requirements{
		Commands: []string{"git", "go"},
		Secrets:  []string{"SSH_AUTH_SOCK"},
	}
	withoutDuplicates.Requirements = Requirements{
		Commands: []string{"git", "go"},
		Secrets:  []string{"TOKEN"},
	}

	got, err := manifest.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() error = %v", err)
	}
	want, err := withoutDuplicates.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() without duplicates error = %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("CanonicalJSON() with duplicates = %s, want %s", got, want)
	}
	if strings.Contains(string(got), `"git","git"`) || strings.Contains(string(got), `"TOKEN","TOKEN"`) {
		t.Fatalf("CanonicalJSON() retained duplicate requirements: %s", got)
	}
}

func TestManifestValidateRules(t *testing.T) {
	valid := Manifest{
		SchemaVersion: 1,
		Name:          "valid",
		Components: []Component{{
			ID:         "config:editor",
			Type:       ComponentTypeConfig,
			Scope:      ScopeUser,
			TrustClass: TrustDeclarative,
			Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
	}
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{name: "empty component ID", mutate: func(manifest *Manifest) { manifest.Components[0].ID = "" }, wantErr: "ID is required"},
		{name: "bad component type", mutate: func(manifest *Manifest) {
			manifest.Components[0].Type = ComponentType("unknown")
			manifest.Components[0].ID = "unknown:editor"
		}, wantErr: "type"},
		{name: "bad scope", mutate: func(manifest *Manifest) { manifest.Components[0].Scope = Scope("workspace") }, wantErr: "scope"},
		{name: "bad trust class", mutate: func(manifest *Manifest) { manifest.Components[0].TrustClass = TrustClass("untrusted") }, wantErr: "trust"},
		{name: "hook declarative trust mismatch", mutate: func(manifest *Manifest) {
			manifest.Components[0].Type = ComponentTypeHook
			manifest.Components[0].ID = "hook:format"
		}, wantErr: "requires trust"},
		{name: "permission policy executable trust mismatch", mutate: func(manifest *Manifest) {
			manifest.Components[0].Type = ComponentTypePermissionPolicy
			manifest.Components[0].ID = "permission-policy:workspace"
			manifest.Components[0].TrustClass = TrustExecutable
		}, wantErr: "requires trust"},
		{name: "duplicate component IDs", mutate: func(manifest *Manifest) {
			manifest.Components = append(manifest.Components, manifest.Components[0])
		}, wantErr: "duplicate component ID"},
		{name: "bad digest when required", mutate: func(manifest *Manifest) {
			manifest.Components[0].Digest = "not-a-digest"
		}, wantErr: "digest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := valid
			manifest.Components = append([]Component(nil), valid.Components...)
			test.mutate(&manifest)
			err := manifest.validate(true)
			if err == nil {
				t.Fatal("validate(true) error = nil, want rejection")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantErr)) {
				t.Fatalf("validate(true) error = %q, want %q", err, test.wantErr)
			}
		})
	}
}

func TestManifestValidateAllowsMissingDigestForBuild(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "pre-build",
		Components: []Component{{
			ID:         "config:editor",
			Type:       ComponentTypeConfig,
			Scope:      ScopeUser,
			TrustClass: TrustDeclarative,
		}},
	}
	if err := manifest.validate(false); err != nil {
		t.Fatalf("validate(false) error = %v, want nil for a pre-build manifest", err)
	}
	if err := manifest.validate(true); err == nil {
		t.Fatal("validate(true) error = nil, want missing digest rejection")
	}
}

func TestJSONMarshalAcceptsPreBuildManifest(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "pre-build",
		Components: []Component{{
			ID:         "config:editor",
			Type:       ComponentTypeConfig,
			Scope:      ScopeUser,
			TrustClass: TrustDeclarative,
		}},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !json.Valid(encoded) {
		t.Fatalf("json.Marshal() returned invalid JSON: %s", encoded)
	}
}

func TestLayerMediaTypes(t *testing.T) {
	tests := []struct {
		componentType ComponentType
		want          string
	}{
		{ComponentTypeConfig, "application/vnd.devm.capsule.config.v1.tar+gzip"},
		{ComponentTypeSkill, "application/vnd.devm.capsule.skill.v1.tar+gzip"},
		{ComponentTypeCommand, "application/vnd.devm.capsule.command.v1.tar+gzip"},
		{ComponentTypeSubagent, "application/vnd.devm.capsule.subagent.v1.tar+gzip"},
		{ComponentTypeHook, "application/vnd.devm.capsule.hook.v1.tar+gzip"},
		{ComponentTypeIntegration, "application/vnd.devm.capsule.integration.v1.tar+gzip"},
		{ComponentTypePermissionPolicy, "application/vnd.devm.capsule.permission-policy.v1.tar+gzip"},
		{ComponentTypeTemplate, "application/vnd.devm.capsule.template.v1.tar+gzip"},
		{ComponentTypeExtension, "application/vnd.devm.capsule.extension.v1.tar+gzip"},
	}

	for _, test := range tests {
		t.Run(string(test.componentType), func(t *testing.T) {
			if got := LayerMediaType(test.componentType); got != test.want {
				t.Fatalf("LayerMediaType(%q) = %q, want %q", test.componentType, got, test.want)
			}
		})
	}
	if ArtifactMediaType != "application/vnd.devm.capsule.v1" {
		t.Fatalf("ArtifactMediaType = %q", ArtifactMediaType)
	}
}

func TestBuildLayerRequiresValidComponentType(t *testing.T) {
	root := writeFixture(t, fixtureFiles{"config.txt": {content: "config\n", mode: 0o644}})
	for _, test := range []struct {
		name          string
		componentType ComponentType
		want          string
	}{
		{name: "missing type", componentType: "", want: "required"},
		{name: "invalid type", componentType: ComponentType("unknown"), want: "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewBuilder(0).BuildLayer(root, test.componentType)
			if err == nil {
				t.Fatal("BuildLayer() error = nil, want component type rejection")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildLayer() error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestBuildLayerRejectsNegativeSourceDateEpoch(t *testing.T) {
	root := writeFixture(t, fixtureFiles{"config.txt": {content: "config\n", mode: 0o644}})
	_, err := NewBuilder(-1).BuildLayer(root, ComponentTypeConfig)
	if err == nil {
		t.Fatal("BuildLayer() error = nil, want negative SourceDateEpoch rejection")
	}
	if !strings.Contains(err.Error(), "source date epoch") || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("BuildLayer() error = %q, want clear negative source date epoch context", err)
	}
}

func TestBuildLayerUsesUSTARHeadersWithoutPAX(t *testing.T) {
	root := writeFixture(t, fixtureFiles{
		"config/settings.json": {content: "{}\n", mode: 0o644},
		"run.sh":               {content: "#!/bin/sh\n", mode: 0o755},
	})
	layer, err := NewBuilder(1700000000).BuildLayer(root, ComponentTypeConfig)
	if err != nil {
		t.Fatalf("BuildLayer() error = %v", err)
	}

	tarBytes := gunzipLayer(t, layer.Bytes)
	for offset := 0; offset+512 <= len(tarBytes); {
		block := tarBytes[offset : offset+512]
		if bytes.Equal(block, make([]byte, 512)) {
			break
		}
		if got, want := string(block[257:263]), "ustar\x00"; got != want {
			t.Fatalf("raw tar header at offset %d has magic %q, want %q", offset, got, want)
		}
		if block[156] == tar.TypeXHeader || block[156] == tar.TypeXGlobalHeader {
			t.Fatalf("raw tar contains PAX header type %q", block[156])
		}
		size, err := parseRawTarSize(block[124:136])
		if err != nil {
			t.Fatalf("parse raw tar size at offset %d: %v", offset, err)
		}
		offset += 512 + int((size+511)/512*512)
	}

	reader, err := gzip.NewReader(bytes.NewReader(layer.Bytes))
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar header: %v", err)
		}
		if header.Typeflag == tar.TypeXHeader || header.Typeflag == tar.TypeXGlobalHeader {
			t.Fatalf("tar reader returned PAX header %q", header.Name)
		}
		if len(header.PAXRecords) != 0 {
			t.Fatalf("tar header %q has PAX records: %v", header.Name, header.PAXRecords)
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Fatalf("tar header %q identity = uid %d gid %d uname %q gname %q, want zero/empty", header.Name, header.Uid, header.Gid, header.Uname, header.Gname)
		}
		if _, err := io.Copy(io.Discard, tarReader); err != nil {
			t.Fatalf("read tar content for %q: %v", header.Name, err)
		}
	}
}

func TestBuildLayerReadsEachFileOnceAndIndexesWrittenBytes(t *testing.T) {
	root := writeFixture(t, fixtureFiles{"config.txt": {content: "original\n", mode: 0o644}})
	originalReadFile := readFile
	readCount := 0
	t.Cleanup(func() { readFile = originalReadFile })
	readFile = func(path string) ([]byte, error) {
		content, err := originalReadFile(path)
		if err != nil {
			return nil, err
		}
		readCount++
		if readCount == 1 {
			if err := os.WriteFile(path, []byte("mutated after index\n"), 0o644); err != nil {
				t.Fatalf("mutate fixture after first read: %v", err)
			}
		}
		return content, nil
	}

	layer, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
	if err != nil {
		t.Fatalf("BuildLayer() error = %v", err)
	}
	if got, want := readCount, 1; got != want {
		t.Fatalf("file read count = %d, want %d", got, want)
	}
	entries := tarEntries(t, layer.Bytes)
	if got, want := string(entries.contents["config.txt"]), "original\n"; got != want {
		t.Fatalf("tar content = %q, want %q", got, want)
	}
	indexEntry, ok := findIndexEntry(layer.Index, "config.txt")
	if !ok {
		t.Fatal("layer index is missing config.txt")
	}
	if got, want := indexEntry.Digest, expectedDigestBytes([]byte("original\n")); got != want {
		t.Fatalf("index digest = %q, want %q", got, want)
	}
}

func TestBuildLayerRejectsNonASCIIAndOverLimitUSTARPaths(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantErrors []string
	}{
		{name: "non ASCII", path: "café.txt", wantErrors: []string{"café.txt", "ASCII"}},
		{name: "name field too long", path: strings.Repeat("a", 101), wantErrors: []string{"USTAR", strings.Repeat("a", 101)}},
		{name: "prefix field too long", path: strings.Repeat("p", 80) + "/" + strings.Repeat("q", 80) + "/" + strings.Repeat("r", 80), wantErrors: []string{"USTAR"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			fullPath := filepath.Join(root, filepath.FromSlash(test.path))
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
				t.Fatalf("mkdir path parent: %v", err)
			}
			if err := os.WriteFile(fullPath, []byte("content\n"), 0o644); err != nil {
				t.Fatalf("write path: %v", err)
			}
			_, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
			if err == nil {
				t.Fatal("BuildLayer() error = nil, want USTAR path rejection")
			}
			for _, want := range test.wantErrors {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("BuildLayer() error = %q, want %q", err, want)
				}
			}
		})
	}
}

func TestBuildLayerRejectsInRootSymlink(t *testing.T) {
	root := t.TempDir()
	insidePath := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(insidePath, []byte("inside\n"), 0o644); err != nil {
		t.Fatalf("write in-root target: %v", err)
	}
	if err := os.Symlink("inside.txt", filepath.Join(root, "alias.txt")); err != nil {
		t.Fatalf("create in-root symlink: %v", err)
	}

	_, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
	if err == nil {
		t.Fatal("BuildLayer() error = nil, want in-root symlink rejection")
	}
	if !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("BuildLayer() error = %q, want unsupported symlink context", err)
	}
}

func TestBuildLayerRejectsHardLinkedFiles(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.txt")
	linkedPath := filepath.Join(root, "linked.txt")
	if err := os.WriteFile(sourcePath, []byte("shared\n"), 0o644); err != nil {
		t.Fatalf("write hard-link source: %v", err)
	}
	if err := os.Link(sourcePath, linkedPath); err != nil {
		t.Fatalf("create hard link: %v", err)
	}

	_, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
	if err == nil {
		t.Fatal("BuildLayer() error = nil, want hard-link rejection")
	}
	if !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("BuildLayer() error = %q, want hard-link context", err)
	}
}

func TestBuildLayerRejectsReservedIndexPath(t *testing.T) {
	root := writeFixture(t, fixtureFiles{IndexPath: {content: "collision\n", mode: 0o644}})
	_, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
	if err == nil {
		t.Fatal("BuildLayer() error = nil, want reserved index path rejection")
	}
	if !strings.Contains(err.Error(), IndexPath) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("BuildLayer() error = %q, want reserved index path context", err)
	}
}

func TestBuildLayerGoldenFixture(t *testing.T) {
	root := writeFixture(t, fixtureFiles{
		"config/settings.json": {content: "{\"name\":\"demo\"}\n", mode: 0o600},
		"bin/run":              {content: "#!/bin/sh\necho ok\n", mode: 0o700},
	})

	layer, err := NewBuilder(1700000000).BuildLayer(root, ComponentTypeSkill)
	if err != nil {
		t.Fatalf("BuildLayer() error = %v", err)
	}
	if got, want := layer.Digest, "sha256:837c0b69a2cd7c407f358ad71dcc261749f568c0056de72e009fd4bf4e9f2a89"; got != want {
		t.Fatalf("layer digest = %q, want %q", got, want)
	}
	if got, want := layer.SizeBytes, int64(len(layer.Bytes)); got != want {
		t.Fatalf("layer size = %d, want %d", got, want)
	}
	if got, want := layer.MediaType, LayerMediaType(ComponentTypeSkill); got != want {
		t.Fatalf("layer media type = %q, want %q", got, want)
	}
	if got, want := string(mustCanonicalIndex(t, layer)), `{"bin/run":{"digest":"sha256:b4d644d4279594903f1a9911956432d9473041f2984fc6014c14d7402c7d126c","mode":493},"config/settings.json":{"digest":"sha256:e55654f16429cee2fc13f1416af140ac945d42fa24d552a93f8e933bb66c07bd","mode":420}}`; got != want {
		t.Fatalf("canonical index = %s, want %s", got, want)
	}

	entries := tarEntries(t, layer.Bytes)
	wantNames := []string{"bin", "bin/run", "config", "config/settings.json", IndexPath}
	if !reflect.DeepEqual(entries.names, wantNames) {
		t.Fatalf("tar entries = %v, want %v", entries.names, wantNames)
	}
	if got, want := string(entries.contents[IndexPath]), string(mustCanonicalIndex(t, layer)); got != want {
		t.Fatalf("embedded index = %s, want %s", got, want)
	}
}

func TestBuildLayerIsDeterministicAcrossIndependentBuilders(t *testing.T) {
	tests := []struct {
		name  string
		epoch int64
	}{
		{name: "explicit source date epoch", epoch: 1700000000},
		{name: "default epoch", epoch: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			firstRoot := writeFixture(t, fixtureFiles{
				"z-last.txt":  {content: "last\n", mode: 0o644},
				"a-first.txt": {content: "first\n", mode: 0o644},
				"bin/run":     {content: "#!/bin/sh\n", mode: 0o755},
			})
			secondRoot := writeFixture(t, fixtureFiles{
				"bin/run":     {content: "#!/bin/sh\n", mode: 0o755},
				"a-first.txt": {content: "first\n", mode: 0o644},
				"z-last.txt":  {content: "last\n", mode: 0o644},
			})

			first, err := NewBuilder(test.epoch).BuildLayer(firstRoot, ComponentTypeConfig)
			if err != nil {
				t.Fatalf("first BuildLayer() error = %v", err)
			}
			second, err := NewBuilder(test.epoch).BuildLayer(secondRoot, ComponentTypeConfig)
			if err != nil {
				t.Fatalf("second BuildLayer() error = %v", err)
			}
			if !bytes.Equal(first.Bytes, second.Bytes) {
				t.Fatal("independent builders produced different layer bytes")
			}
			if first.Digest != second.Digest {
				t.Fatalf("layer digests differ: %q != %q", first.Digest, second.Digest)
			}
		})
	}
}

func TestBuildLayerRejectsSymlinkEscapingComponentRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsidePath, []byte("not component content"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(root, "escaped.txt")); err != nil {
		t.Fatalf("create escaping symlink: %v", err)
	}

	_, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
	if err == nil {
		t.Fatal("BuildLayer() error = nil, want symlink escape rejection")
	}
	if !strings.Contains(err.Error(), "escapes component root") {
		t.Fatalf("BuildLayer() error = %q, want component-root context", err)
	}
}

func TestBuildLayerNormalizesModes(t *testing.T) {
	root := writeFixture(t, fixtureFiles{
		"private.txt": {content: "private\n", mode: 0o600},
		"execute.txt": {content: "execute\n", mode: 0o700},
	})

	layer, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
	if err != nil {
		t.Fatalf("BuildLayer() error = %v", err)
	}
	wantModes := map[string]int64{"private.txt": 0o644, "execute.txt": 0o755}
	for path, want := range wantModes {
		entry, ok := findIndexEntry(layer.Index, path)
		if !ok {
			t.Fatalf("index is missing %q", path)
		}
		if entry.Mode != uint32(want) {
			t.Fatalf("index mode for %q = %o, want %o", path, entry.Mode, want)
		}
	}
	entries := tarEntries(t, layer.Bytes)
	for path, want := range wantModes {
		if got := entries.modes[path]; got != want {
			t.Fatalf("tar mode for %q = %o, want %o", path, got, want)
		}
	}
}

func TestBuildLayerRejectsEmptyComponent(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{name: "empty directory", setup: func(*testing.T, string) {}},
		{name: "only empty directory", setup: func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
				t.Fatalf("mkdir empty directory: %v", err)
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			_, err := NewBuilder(0).BuildLayer(root, ComponentTypeConfig)
			if err == nil {
				t.Fatal("BuildLayer() error = nil, want empty-component rejection")
			}
			if !strings.Contains(err.Error(), "component is empty") {
				t.Fatalf("BuildLayer() error = %q, want empty-component context", err)
			}
		})
	}
}

func TestBuildCapsuleIncludesLayerDigestsInCanonicalManifest(t *testing.T) {
	skillRoot := writeFixture(t, fixtureFiles{"run.sh": {content: "#!/bin/sh\n", mode: 0o755}})
	configRoot := writeFixture(t, fixtureFiles{"settings.json": {content: "{}\n", mode: 0o644}})
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "demo",
		Components: []Component{
			{ID: "skill:run", Type: ComponentTypeSkill, Scope: ScopeUser, TrustClass: TrustExecutable},
			{ID: "config:settings", Type: ComponentTypeConfig, Scope: ScopeProject, TrustClass: TrustDeclarative},
		},
	}

	capsule, err := NewBuilder(1700000000).Build(manifest, map[string]string{
		"skill:run":       skillRoot,
		"config:settings": configRoot,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got, want := len(capsule.Layers), len(manifest.Components); got != want {
		t.Fatalf("layer count = %d, want %d", got, want)
	}
	if got, want := len(capsule.Manifest.Components), len(manifest.Components); got != want {
		t.Fatalf("manifest component count = %d, want %d", got, want)
	}
	for index, layer := range capsule.Layers {
		component := capsule.Manifest.Components[index]
		if component.Digest != layer.Digest {
			t.Errorf("component %q digest = %q, want layer digest %q", component.ID, component.Digest, layer.Digest)
		}
		if component.SizeBytes != int64(len(layer.Bytes)) {
			t.Errorf("component %q size = %d, want %d", component.ID, component.SizeBytes, len(layer.Bytes))
		}
		if component.MediaType != layer.MediaType {
			t.Errorf("component %q media type = %q, want layer media type %q", component.ID, component.MediaType, layer.MediaType)
		}
	}
	if got, want := capsule.Digest, "sha256:b65d6019b6b5bdc0000ad2e11dda5e247465cf6f7f2ae9dbdf5e53b542694575"; got != want {
		t.Fatalf("capsule digest = %q, want %q", got, want)
	}
	canonical, err := capsule.Manifest.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical capsule manifest: %v", err)
	}
	if got := expectedDigestBytes(canonical); got != capsule.Digest {
		t.Fatalf("capsule digest = %q, does not hash canonical manifest %q", capsule.Digest, got)
	}
}

func TestBuildAcceptsPreSetDigestsAndReplacesThem(t *testing.T) {
	root := writeFixture(t, fixtureFiles{"settings.json": {content: "{}\n", mode: 0o644}})
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "pre-built",
		Components: []Component{{
			ID:         "config:settings",
			Type:       ComponentTypeConfig,
			Scope:      ScopeProject,
			TrustClass: TrustDeclarative,
			MediaType:  LayerMediaType(ComponentTypeConfig),
			Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SizeBytes:  123,
		}},
	}
	capsule, err := (Builder{}).Build(manifest, map[string]string{"config:settings": root})
	if err != nil {
		t.Fatalf("Build() error = %v, want validate(false) to accept a pre-set valid digest", err)
	}
	if capsule.Manifest.Components[0].Digest == manifest.Components[0].Digest {
		t.Fatal("Build() retained the pre-set digest instead of the built layer digest")
	}
	if capsule.Manifest.Components[0].Digest != capsule.Layers[0].Digest {
		t.Fatal("Build() manifest digest does not match the built layer digest")
	}
}

func TestBuildRejectsInvalidPreSetDigest(t *testing.T) {
	root := writeFixture(t, fixtureFiles{"settings.json": {content: "{}\n", mode: 0o644}})
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "invalid-pre-built",
		Components: []Component{{
			ID:         "config:settings",
			Type:       ComponentTypeConfig,
			Scope:      ScopeProject,
			TrustClass: TrustDeclarative,
			Digest:     "not-a-digest",
		}},
	}
	_, err := (Builder{}).Build(manifest, map[string]string{"config:settings": root})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("Build() error = %q, want invalid pre-set digest rejection", err)
	}
}

func TestBuildRejectsDuplicateComponentDirectories(t *testing.T) {
	root := writeFixture(t, fixtureFiles{"shared.txt": {content: "shared\n", mode: 0o644}})
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "duplicate-directories",
		Components: []Component{
			{ID: "config:first", Type: ComponentTypeConfig, Scope: ScopeUser, TrustClass: TrustDeclarative},
			{ID: "skill:second", Type: ComponentTypeSkill, Scope: ScopeUser, TrustClass: TrustDeclarative},
		},
	}
	_, err := (Builder{}).Build(manifest, map[string]string{
		"config:first": root,
		"skill:second": root,
	})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("Build() error = %q, want duplicate component directory rejection", err)
	}
}

func TestBuildRejectsMismatchedComponentDirectoryKeys(t *testing.T) {
	root := writeFixture(t, fixtureFiles{"settings.json": {content: "{}\n", mode: 0o644}})
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "mismatched-directories",
		Components: []Component{
			{ID: "config:first", Type: ComponentTypeConfig, Scope: ScopeUser, TrustClass: TrustDeclarative},
			{ID: "config:second", Type: ComponentTypeConfig, Scope: ScopeUser, TrustClass: TrustDeclarative},
		},
	}
	_, err := (Builder{}).Build(manifest, map[string]string{
		"config:first": root,
		"config:other": root,
	})
	if err == nil || !strings.Contains(err.Error(), "config:second") {
		t.Fatalf("Build() error = %q, want missing config:second directory context", err)
	}
}

func TestComputeCapsuleDigestIsCanonical(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: 1,
		Name:          "digest-test",
		Components: []Component{
			{
				ID:         "config:a",
				Type:       ComponentTypeConfig,
				Scope:      ScopeUser,
				TrustClass: TrustDeclarative,
				MediaType:  LayerMediaType(ComponentTypeConfig),
				Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SizeBytes:  1,
			},
		},
	}

	got, err := ComputeCapsuleDigest(manifest)
	if err != nil {
		t.Fatalf("ComputeCapsuleDigest() error = %v", err)
	}
	canonical, err := manifest.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() error = %v", err)
	}
	if want := expectedDigestBytes(canonical); got != want {
		t.Fatalf("ComputeCapsuleDigest() = %q, want %q", got, want)
	}
}

type fixtureFile struct {
	content string
	mode    os.FileMode
}

type fixtureFiles map[string]fixtureFile

func writeFixture(t *testing.T, files fixtureFiles) string {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		file := files[path]
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", path, err)
		}
		if err := os.WriteFile(fullPath, []byte(file.content), file.mode); err != nil {
			t.Fatalf("write %q: %v", path, err)
		}
		if err := os.Chmod(fullPath, file.mode); err != nil {
			t.Fatalf("chmod %q: %v", path, err)
		}
	}
	return root
}

type tarEntrySet struct {
	names    []string
	modes    map[string]int64
	contents map[string][]byte
}

func tarEntries(t *testing.T, compressed []byte) tarEntrySet {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("NewLayerReader() error = %v", err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	entries := tarEntrySet{modes: make(map[string]int64), contents: make(map[string][]byte)}
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		entries.names = append(entries.names, header.Name)
		entries.modes[header.Name] = header.Mode
		content, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("read tar content for %q: %v", header.Name, err)
		}
		entries.contents[header.Name] = content
	}
	return entries
}

func findIndexEntry(entries []FileIndexEntry, path string) (FileIndexEntry, bool) {
	for _, entry := range entries {
		if entry.Path == path {
			return entry, true
		}
	}
	return FileIndexEntry{}, false
}

func mustCanonicalIndex(t *testing.T, layer Layer) []byte {
	t.Helper()
	index, err := layer.CanonicalIndexJSON()
	if err != nil {
		t.Fatalf("CanonicalIndexJSON() error = %v", err)
	}
	return index
}

func expectedDigestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func gunzipLayer(t *testing.T, compressed []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip content: %v", err)
	}
	return content
}

func parseRawTarSize(field []byte) (int64, error) {
	value := strings.Trim(string(field), " \x00")
	if value == "" {
		return 0, nil
	}
	size, err := strconv.ParseInt(value, 8, 64)
	if err != nil {
		return 0, err
	}
	return size, nil
}

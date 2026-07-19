package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

var capsuleCLINamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// RunCapsuleCapture scans local candidates and prints an explicit component
// selection plan. It does not package, upload, authenticate, or prompt.
func RunCapsuleCapture(ctx context.Context, profileRoot string, selectors []profile.Selector, output io.Writer) error {
	_ = ctx
	candidates, err := profile.Scan(profileRoot)
	if err != nil {
		return fmt.Errorf("scan Capsule candidates: %w", err)
	}
	selected, err := profile.Select(profileRoot, selectors)
	if err != nil {
		return fmt.Errorf("resolve Capsule selections: %w", err)
	}
	items := captureItems(candidates, selected)
	return writeCaptureGroups(items, output)
}

// RunCapsuleBuild compiles explicit selections into a local Capsule and
// prints only its manifest and Component layer digests.
func RunCapsuleBuild(ctx context.Context, profileRoot string, selectors []profile.Selector, output io.Writer) error {
	_ = ctx
	capsule, err := profile.Compile(profileRoot, selectors)
	if err != nil {
		return fmt.Errorf("build Capsule: %w", err)
	}
	var report strings.Builder
	fmt.Fprintf(&report, "manifest_digest %s\n", capsule.Digest)
	for _, component := range capsule.Manifest.Components {
		fmt.Fprintf(&report, "component id=%q digest=%s size_bytes=%d type=%s scope=%s trust=%s\n", component.ID, component.Digest, component.SizeBytes, component.Type, component.Scope, component.TrustClass)
	}
	if _, err := io.WriteString(output, report.String()); err != nil {
		return fmt.Errorf("write Capsule build: %w", err)
	}
	return nil
}

type capsuleRemoteCommand struct {
	api     *contracts.ClientWithResponses
	grants  oci.GrantProvider
	ownerID string
	token   string
	output  io.Writer
}

func (command capsuleRemoteCommand) publish(ctx context.Context, target, profileRoot string, selectors []profile.Selector) error {
	name, tag, err := parseCapsuleTag(target)
	if err != nil {
		return err
	}
	built, err := profile.CompileNamed(profileRoot, name, selectors)
	if err != nil {
		return fmt.Errorf("build Capsule for publish: %w", err)
	}
	client, err := oci.NewClient(command.ownerID, command.grants)
	if err != nil {
		return fmt.Errorf("configure Capsule publish: %w", err)
	}
	publication, err := client.Publish(ctx, built)
	if err != nil {
		return err
	}
	response, err := command.api.PutCapsuleTagWithResponse(ctx, name, tag, contracts.PutCapsuleTagRequest{Digest: publication.CapsuleDigest}, bearerRequestEditor(command.token))
	if err != nil {
		return fmt.Errorf("publish Capsule tag: %w", err)
	}
	if response == nil || response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Digest != publication.CapsuleDigest || response.JSON200.Name != name || response.JSON200.Tag != tag {
		return errors.New("publish Capsule tag: control plane returned an invalid response")
	}
	output := command.output
	if output == nil {
		output = io.Discard
	}
	_, err = fmt.Fprintf(output, "digest %s\ntag %s digest=%s updated_at=%s\n", publication.CapsuleDigest, contracts.FormatOwnedCapsuleTagRef(command.ownerID, name, tag), response.JSON200.Digest, response.JSON200.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
	if err != nil {
		return errors.New("write Capsule publish result")
	}
	return nil
}

func (command capsuleRemoteCommand) inspect(ctx context.Context, ref string) error {
	digest, err := command.resolve(ctx, ref)
	if err != nil {
		return err
	}
	client, err := oci.NewClient(command.ownerID, command.grants)
	if err != nil {
		return fmt.Errorf("configure Capsule inspect: %w", err)
	}
	manifest, err := client.ResolveManifest(ctx, digest)
	if err != nil {
		return fmt.Errorf("inspect Capsule: %w", err)
	}
	output := command.output
	if output == nil {
		output = io.Discard
	}
	var report strings.Builder
	fmt.Fprintf(&report, "manifest name=%q digest=%s schema_version=%d\n", manifest.Name, digest, manifest.SchemaVersion)
	for _, component := range manifest.Components {
		fmt.Fprintf(&report, "component id=%q type=%s scope=%s trust=%s digest=%s\n", component.ID, component.Type, component.Scope, component.TrustClass, component.Digest)
	}
	if _, err := io.WriteString(output, report.String()); err != nil {
		return errors.New("write Capsule inspect result")
	}
	return nil
}

func (command capsuleRemoteCommand) diff(ctx context.Context, fromRef, toRef string) error {
	fromDigest, err := command.resolve(ctx, fromRef)
	if err != nil {
		return fmt.Errorf("resolve from Capsule: %w", err)
	}
	toDigest, err := command.resolve(ctx, toRef)
	if err != nil {
		return fmt.Errorf("resolve to Capsule: %w", err)
	}
	client, err := oci.NewClient(command.ownerID, command.grants)
	if err != nil {
		return fmt.Errorf("configure Capsule diff: %w", err)
	}
	fromManifest, err := client.ResolveManifest(ctx, fromDigest)
	if err != nil {
		return fmt.Errorf("inspect from Capsule: %w", err)
	}
	toManifest, err := client.ResolveManifest(ctx, toDigest)
	if err != nil {
		return fmt.Errorf("inspect to Capsule: %w", err)
	}
	report := capsuleComponentDiff(fromManifest, toManifest)
	output := command.output
	if output == nil {
		output = io.Discard
	}
	if _, err := io.WriteString(output, report); err != nil {
		return errors.New("write Capsule diff result")
	}
	return nil
}

func (command capsuleRemoteCommand) resolve(ctx context.Context, ref string) (string, error) {
	if capsuleGrantDigestPattern.MatchString(ref) {
		return ref, nil
	}
	if parsed, err := contracts.ParseOwnedCapsuleRef(ref); err == nil {
		if parsed.OwnerID != command.ownerID {
			return "", errors.New("resolve Capsule ref: ref does not belong to the authenticated User")
		}
		if parsed.Digest != "" {
			return parsed.Digest, nil
		}
		return command.resolveTag(ctx, parsed.Name, parsed.Tag)
	}
	name, tag, err := parseCapsuleTag(ref)
	if err != nil {
		return "", errors.New("resolve Capsule ref: expected a digest, owner-scoped ref, or name:tag")
	}
	return command.resolveTag(ctx, name, tag)
}

func (command capsuleRemoteCommand) resolveTag(ctx context.Context, name, tag string) (string, error) {
	response, err := command.api.GetCapsuleTagWithResponse(ctx, name, tag, bearerRequestEditor(command.token))
	if err != nil {
		return "", fmt.Errorf("resolve Capsule tag: %w", err)
	}
	if response == nil || response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Name != name || response.JSON200.Tag != tag || !capsuleGrantDigestPattern.MatchString(response.JSON200.Digest) {
		return "", errors.New("resolve Capsule tag: control plane returned an invalid response")
	}
	return response.JSON200.Digest, nil
}

func parseCapsuleTag(value string) (string, string, error) {
	name, tag, found := strings.Cut(value, ":")
	if !found || strings.Contains(tag, ":") || !capsuleCLINamePattern.MatchString(name) || !capsuleCLINamePattern.MatchString(tag) {
		return "", "", errors.New("Capsule tag must be canonical name:tag")
	}
	return name, tag, nil
}

type capsuleComponentKey struct {
	id, componentType, scope string
}

func capsuleComponentDiff(from, to capsule.Manifest) string {
	fromComponents := make(map[capsuleComponentKey]capsule.Component, len(from.Components))
	toComponents := make(map[capsuleComponentKey]capsule.Component, len(to.Components))
	keys := make(map[capsuleComponentKey]struct{}, len(from.Components)+len(to.Components))
	for _, component := range from.Components {
		key := capsuleComponentKey{component.ID, string(component.Type), string(component.Scope)}
		fromComponents[key], keys[key] = component, struct{}{}
	}
	for _, component := range to.Components {
		key := capsuleComponentKey{component.ID, string(component.Type), string(component.Scope)}
		toComponents[key], keys[key] = component, struct{}{}
	}
	ordered := make([]capsuleComponentKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].id != ordered[j].id {
			return ordered[i].id < ordered[j].id
		}
		if ordered[i].componentType != ordered[j].componentType {
			return ordered[i].componentType < ordered[j].componentType
		}
		return ordered[i].scope < ordered[j].scope
	})
	var report strings.Builder
	for _, key := range ordered {
		before, hadBefore := fromComponents[key]
		after, hasAfter := toComponents[key]
		switch {
		case !hadBefore:
			fmt.Fprintf(&report, "added id=%q type=%s scope=%s digest=%s\n", key.id, key.componentType, key.scope, after.Digest)
		case !hasAfter:
			fmt.Fprintf(&report, "removed id=%q type=%s scope=%s digest=%s\n", key.id, key.componentType, key.scope, before.Digest)
		case before.Digest != after.Digest:
			fmt.Fprintf(&report, "changed id=%q type=%s scope=%s from=%s to=%s\n", key.id, key.componentType, key.scope, before.Digest, after.Digest)
		}
	}
	if report.Len() == 0 {
		report.WriteString("no component changes\n")
	}
	return report.String()
}

type captureItem struct {
	candidate profile.Candidate
	selected  bool
}

func captureItems(candidates, selected []profile.Candidate) []captureItem {
	selectedByPath := make(map[string][]profile.Candidate, len(selected))
	for _, candidate := range selected {
		selectedByPath[candidate.Path] = append(selectedByPath[candidate.Path], candidate)
	}
	items := make([]captureItem, 0, len(candidates)+len(selected))
	for _, candidate := range candidates {
		selectedItems := selectedByPath[candidate.Path]
		if len(selectedItems) == 0 {
			items = append(items, captureItem{candidate: candidate})
			continue
		}
		for _, selectedCandidate := range selectedItems {
			items = append(items, captureItem{candidate: selectedCandidate, selected: true})
		}
		delete(selectedByPath, candidate.Path)
	}
	for _, remaining := range selectedByPath {
		for _, candidate := range remaining {
			items = append(items, captureItem{candidate: candidate, selected: true})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].candidate.Component.ID < items[j].candidate.Component.ID
	})
	return items
}

func writeCaptureGroups(items []captureItem, output io.Writer) error {
	var report strings.Builder
	report.WriteString(captureGroupsString(items))
	if _, err := io.WriteString(output, report.String()); err != nil {
		return fmt.Errorf("write Capsule capture: %w", err)
	}
	return nil
}

func captureGroupsString(items []captureItem) string {
	var report strings.Builder
	groups := []string{"safe", "review", "requires_authorization", "excluded", "conflict"}
	for _, group := range groups {
		fmt.Fprintf(&report, "%s:\n", group)
		for _, item := range items {
			if candidateGroup(item.candidate) != group {
				continue
			}
			evidence := item.candidate.Evidence
			if !item.selected && item.candidate.Disposition != "excluded" {
				evidence = "not_selected+" + evidence
			}
			writeComponentItem(&report, item.candidate, evidence, item.selected)
		}
	}
	return report.String()
}

func candidateGroup(candidate profile.Candidate) string {
	switch candidate.Disposition {
	case "safe", "review", "requires_authorization", "excluded":
		return candidate.Disposition
	default:
		return "conflict"
	}
}

func writeComponentItem(output *strings.Builder, candidate profile.Candidate, evidence string, selected bool) {
	component := candidate.Component
	fmt.Fprintf(output, "  component=%s type=%s scope=%s trust=%s path=%q selector=%q selected=%t evidence=%s sensitivity=%s contains_executable=%t", component.ID, component.Type, component.Scope, component.TrustClass, candidate.Path, candidate.Selector, selected, evidence, candidate.Sensitivity, candidate.ContainsExecutable)
	if len(component.Requirements.Secrets) > 0 {
		fmt.Fprintf(output, " requirements_secrets=%q", strings.Join(component.Requirements.Secrets, ","))
	}
	if candidate.SourceDigest != "" {
		fmt.Fprintf(output, " source_digest=%s content_digest=%s", candidate.SourceDigest, candidate.ContentDigest)
	}
	output.WriteByte('\n')
}

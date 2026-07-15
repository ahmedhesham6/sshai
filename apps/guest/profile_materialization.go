package guest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

type MaterializationMode string

const (
	MaterializationManaged    MaterializationMode = "managed"
	MaterializationSeeded     MaterializationMode = "seeded"
	MaterializationReferenced MaterializationMode = "referenced"
)

type MaterializationRoot string

const (
	MaterializationHome             MaterializationRoot = "home"
	MaterializationWorkspace        MaterializationRoot = "workspace"
	materializationFileAdapter                          = "file"
	materializationReferenceAdapter                     = "reference"
	materializationAdapterVersion                       = "v1"
)

type ProfileMaterialization struct {
	ID                string
	ArtifactID        string
	Artifact          *profile.Artifact
	ContentSize       int64
	Mode              MaterializationMode
	Root              MaterializationRoot
	Target            string
	Selector          string
	LastAppliedDigest string
	ObservedDigest    string
	RequirementState  profile.RequirementState
}

type ProfileMaterializationBatch struct {
	HomeRoot      string
	WorkspaceRoot string
	Intent        profile.PlanIntent
	Items         []ProfileMaterialization
}

type ProfileMaterializationResult struct {
	ID                string
	ArtifactID        string
	Mode              MaterializationMode
	Adapter           string
	AdapterVersion    string
	Root              MaterializationRoot
	Target            string
	Selector          string
	DesiredDigest     string
	LastAppliedDigest string
	ObservedDigest    string
	Operation         profile.PlanOperation
	RequirementState  profile.RequirementState
}

var profileSHA256 = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var ErrProfileMaterializationBlocked = errors.New("Profile Materialization is blocked")

type plannedProfileMaterialization struct {
	result  ProfileMaterializationResult
	root    *os.Root
	content []byte
	mode    os.FileMode
}

func ApplyProfileMaterializations(batch ProfileMaterializationBatch) ([]ProfileMaterializationResult, error) {
	if batch.Intent != profile.IntentReconcile && batch.Intent != profile.IntentPrune {
		return nil, fmt.Errorf("apply Profile Materializations: unsupported intent %q", batch.Intent)
	}
	roots := make(map[MaterializationRoot]*os.Root, 2)
	defer func() {
		for _, root := range roots {
			_ = root.Close()
		}
	}()
	seen := make(map[string]struct{}, len(batch.Items))
	ownedTargets := make(map[string][]string, len(batch.Items))
	plans := make([]plannedProfileMaterialization, 0, len(batch.Items))
	for _, item := range batch.Items {
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, fmt.Errorf("apply Profile Materializations: duplicate identity %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		if err := recordMaterializationOwnership(ownedTargets, item); err != nil {
			return nil, fmt.Errorf("apply Profile Materializations: %w", err)
		}
		plan, err := prepareProfileMaterialization(batch, roots, item)
		if err != nil {
			return nil, fmt.Errorf("apply Profile Materializations: plan %q: %w", item.ID, err)
		}
		plans = append(plans, plan)
	}
	results := materializationResults(plans)
	for _, plan := range plans {
		if plan.blocked() {
			return results, fmt.Errorf("apply Profile Materializations: %w: %q requires %s resolution", ErrProfileMaterializationBlocked, plan.result.ID, plan.result.Operation)
		}
	}
	for index := range plans {
		if err := plans[index].apply(); err != nil {
			return materializationResults(plans), fmt.Errorf("apply Profile Materializations: %w", err)
		}
	}
	return materializationResults(plans), nil
}

func prepareProfileMaterialization(
	batch ProfileMaterializationBatch,
	roots map[MaterializationRoot]*os.Root,
	item ProfileMaterialization,
) (plannedProfileMaterialization, error) {
	if item.Artifact == nil {
		operation, observed, err := planRemovedMaterialization(batch, roots, item)
		return plannedProfileMaterialization{
			result: materializationResult(item, operation, item.LastAppliedDigest, observed),
			root:   roots[item.Root],
		}, err
	}
	if item.Mode == MaterializationReferenced {
		operation, err := planReferencedMaterialization(item, batch.Intent)
		return plannedProfileMaterialization{
			result: materializationResult(item, operation, item.LastAppliedDigest, item.ObservedDigest),
		}, err
	}
	if item.Mode != MaterializationManaged && item.Mode != MaterializationSeeded {
		return plannedProfileMaterialization{}, fmt.Errorf("unsupported mode %q", item.Mode)
	}
	if err := validateDesiredMaterialization(item); err != nil {
		return plannedProfileMaterialization{}, err
	}
	root, err := batchMaterializationRoot(batch, roots, item.Root)
	if err != nil {
		return plannedProfileMaterialization{}, err
	}
	observed, err := observeMaterializedFile(root, item.Target, item.Artifact.Selector)
	if err != nil {
		return plannedProfileMaterialization{}, fmt.Errorf("observe: %w", err)
	}
	if observed != item.ObservedDigest {
		return plannedProfileMaterialization{}, fmt.Errorf("%w: observation changed", ErrProfileMaterializationBlocked)
	}
	operation, err := planFileMaterialization(item, observed, batch.Intent)
	return plannedProfileMaterialization{
		result: materializationResult(item, operation, item.LastAppliedDigest, observed),
		root:   root, content: append([]byte(nil), item.Artifact.Content...), mode: os.FileMode(item.Artifact.Mode),
	}, err
}

func (plan plannedProfileMaterialization) blocked() bool {
	return plan.result.Operation == profile.OperationDrift || plan.result.Operation == profile.OperationConflict || plan.result.Operation == profile.OperationRequiresInput
}

func (plan *plannedProfileMaterialization) apply() error {
	operation := plan.result.Operation
	if operation == profile.OperationSkip || operation == profile.OperationOrphan {
		return nil
	}
	if plan.result.Adapter == materializationReferenceAdapter {
		plan.result.LastAppliedDigest = ""
		plan.result.ObservedDigest = ""
		return nil
	}
	observed, err := observeMaterializedFile(plan.root, plan.result.Target, plan.result.Selector)
	if err != nil || observed != plan.result.ObservedDigest {
		return fmt.Errorf("%w: observation for %q changed before mutation", ErrProfileMaterializationBlocked, plan.result.ID)
	}
	if operation == profile.OperationRemove {
		if plan.result.Selector == "$" {
			err = plan.root.Remove(plan.result.Target)
		} else {
			err = removeJSONSelection(plan.root, plan.result.Target, plan.result.Selector)
		}
		if err != nil {
			return fmt.Errorf("remove %q: %w", plan.result.ID, err)
		}
		plan.result.LastAppliedDigest = ""
		plan.result.ObservedDigest = ""
		return nil
	}
	content := plan.content
	if plan.result.Selector != "$" {
		content, err = mergeJSONSelection(plan.root, plan.result.Target, plan.result.Selector, content)
		if err != nil {
			return fmt.Errorf("merge %q: %w", plan.result.ID, err)
		}
	}
	exists, err := materializedFileExists(plan.root, plan.result.Target)
	if err != nil {
		return fmt.Errorf("inspect %q before mutation: %w", plan.result.ID, err)
	}
	if err := writeMaterializedFile(plan.root, plan.result.Target, content, plan.mode, !exists); err != nil {
		return fmt.Errorf("%s %q: %w", operation, plan.result.ID, err)
	}
	plan.result.LastAppliedDigest = plan.result.DesiredDigest
	plan.result.ObservedDigest = plan.result.DesiredDigest
	return nil
}

func materializationResults(plans []plannedProfileMaterialization) []ProfileMaterializationResult {
	results := make([]ProfileMaterializationResult, len(plans))
	for index := range plans {
		results[index] = plans[index].result
	}
	return results
}

func recordMaterializationOwnership(owned map[string][]string, item ProfileMaterialization) error {
	selector := materializationSelector(item)
	key := string(item.Root) + "\x00" + item.Target
	for _, existing := range owned[key] {
		overlaps := selector == existing || selector == "$" || existing == "$" ||
			strings.HasPrefix(selector, existing+".") || strings.HasPrefix(existing, selector+".")
		if overlaps {
			return fmt.Errorf("target %q has overlapping selectors %q and %q", item.Target, existing, selector)
		}
	}
	owned[key] = append(owned[key], selector)
	return nil
}

func planRemovedMaterialization(
	batch ProfileMaterializationBatch,
	roots map[MaterializationRoot]*os.Root,
	item ProfileMaterialization,
) (profile.PlanOperation, string, error) {
	if err := validateRecordedMaterialization(item); err != nil {
		return "", "", err
	}
	lastApplied, err := materializationDigestState(item.LastAppliedDigest)
	if err != nil {
		return "", "", err
	}
	observed := item.ObservedDigest
	if item.Mode != MaterializationReferenced {
		root, err := batchMaterializationRoot(batch, roots, item.Root)
		if err != nil {
			return "", "", err
		}
		observed, err = observeMaterializedFile(root, item.Target, item.Selector)
		if err != nil {
			return "", "", err
		}
		if observed != item.ObservedDigest {
			return "", "", fmt.Errorf("%w: observation changed", ErrProfileMaterializationBlocked)
		}
	}
	observedState, err := materializationDigestState(observed)
	if err != nil {
		return "", "", err
	}
	var snapshot profile.MaterializationSnapshot
	switch item.Mode {
	case MaterializationManaged:
		snapshot = profile.NewManagedMaterialization(profile.AbsentDigest(), lastApplied, observedState)
	case MaterializationSeeded:
		snapshot = profile.NewSeededMaterialization(profile.AbsentDigest(), lastApplied, observedState)
	case MaterializationReferenced:
		snapshot, err = profile.NewReferencedMaterialization(profile.AbsentDigest(), observedState, item.RequirementState)
		if err != nil {
			return "", "", err
		}
	default:
		return "", "", fmt.Errorf("unsupported materialization mode %q", item.Mode)
	}
	operation, err := profile.PlanMaterialization(snapshot, batch.Intent)
	return operation, observed, err
}

func validateRecordedMaterialization(item ProfileMaterialization) error {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ArtifactID) == "" {
		return errors.New("materialization identity is required")
	}
	if item.ContentSize != 0 {
		return errors.New("removed materialization cannot contain content metadata")
	}
	if err := validateMaterializationTarget(item.Target); err != nil {
		return err
	}
	if err := validateMaterializationSelectorSyntax(item.Target, item.Selector); err != nil {
		return fmt.Errorf("materialization %q: %w", item.ID, err)
	}
	if _, err := materializationDigestState(item.LastAppliedDigest); err != nil {
		return err
	}
	if _, err := materializationDigestState(item.ObservedDigest); err != nil {
		return err
	}
	return nil
}

func planReferencedMaterialization(item ProfileMaterialization, intent profile.PlanIntent) (profile.PlanOperation, error) {
	if err := validateReferencedMaterialization(item); err != nil {
		return "", err
	}
	desired, err := materializationDigestState(item.Artifact.ContentDigest)
	if err != nil {
		return "", err
	}
	observed, err := materializationDigestState(item.ObservedDigest)
	if err != nil {
		return "", err
	}
	snapshot, err := profile.NewReferencedMaterialization(desired, observed, item.RequirementState)
	if err != nil {
		return "", err
	}
	return profile.PlanMaterialization(snapshot, intent)
}

func validateReferencedMaterialization(item ProfileMaterialization) error {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ArtifactID) == "" {
		return errors.New("materialization identity is required")
	}
	if len(item.Artifact.Content) != 0 || item.ContentSize != 0 || item.LastAppliedDigest != "" {
		return fmt.Errorf("referenced materialization %q must contain metadata only", item.ID)
	}
	if !profileSHA256.MatchString(item.Artifact.ContentDigest) {
		return fmt.Errorf("referenced materialization %q digest is invalid", item.ID)
	}
	if err := validateMaterializationTarget(item.Target); err != nil {
		return fmt.Errorf("referenced materialization %q: %w", item.ID, err)
	}
	if item.Artifact.Path != item.Target || item.Artifact.Selector != "$" || item.Artifact.SourceLocator != item.Target+"#$" {
		return fmt.Errorf("referenced materialization %q metadata is inconsistent", item.ID)
	}
	return nil
}

func planFileMaterialization(item ProfileMaterialization, observed string, intent profile.PlanIntent) (profile.PlanOperation, error) {
	desired, err := materializationDigestState(item.Artifact.ContentDigest)
	if err != nil {
		return "", err
	}
	lastApplied, err := materializationDigestState(item.LastAppliedDigest)
	if err != nil {
		return "", err
	}
	observedState, err := materializationDigestState(observed)
	if err != nil {
		return "", err
	}
	var snapshot profile.MaterializationSnapshot
	switch item.Mode {
	case MaterializationManaged:
		snapshot = profile.NewManagedMaterialization(desired, lastApplied, observedState)
	case MaterializationSeeded:
		snapshot = profile.NewSeededMaterialization(desired, lastApplied, observedState)
	default:
		return "", fmt.Errorf("unsupported materialization mode %q", item.Mode)
	}
	return profile.PlanMaterialization(snapshot, intent)
}

func validateDesiredMaterialization(item ProfileMaterialization) error {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ArtifactID) == "" {
		return errors.New("materialization identity is required")
	}
	if err := validateProfileArtifactKind(*item.Artifact); err != nil {
		return fmt.Errorf("materialization %q: %w", item.ID, err)
	}
	if item.ContentSize < 0 || int64(len(item.Artifact.Content)) != item.ContentSize {
		return fmt.Errorf("materialization %q content size mismatch", item.ID)
	}
	if !profileSHA256.MatchString(item.Artifact.ContentDigest) || materializationContentDigest(item.Artifact.Content) != item.Artifact.ContentDigest {
		return fmt.Errorf("materialization %q content digest mismatch", item.ID)
	}
	if item.Artifact.Mode > 0o777 {
		return fmt.Errorf("materialization %q mode is invalid", item.ID)
	}
	if err := validateMaterializationTarget(item.Target); err != nil {
		return fmt.Errorf("materialization %q: %w", item.ID, err)
	}
	if item.Artifact.Path != item.Target || item.Artifact.SourceLocator != item.Target+"#"+item.Artifact.Selector {
		return fmt.Errorf("materialization %q artifact target metadata is inconsistent", item.ID)
	}
	if err := validateMaterializationSelector(item.Target, item.Artifact.Selector, item.Artifact.Content); err != nil {
		return fmt.Errorf("materialization %q: %w", item.ID, err)
	}
	return nil
}

func validateProfileArtifactKind(artifact profile.Artifact) error {
	kind := domain.ArtifactKind(artifact.Kind)
	switch kind {
	case domain.ArtifactAgentInstruction, domain.ArtifactCodexSettings, domain.ArtifactClaudeSettings,
		domain.ArtifactShellPreferences, domain.ArtifactGitPreferences, domain.ArtifactSkillInstruction,
		domain.ArtifactSkillExecutable:
	default:
		return fmt.Errorf("unsupported Profile Artifact kind %q", artifact.Kind)
	}
	executable := kind == domain.ArtifactShellPreferences || kind == domain.ArtifactSkillExecutable
	if artifact.ContainsExecutable != executable {
		return errors.New("Profile Artifact executable classification is inconsistent")
	}
	return nil
}

func validateMaterializationSelector(target, selector string, content []byte) error {
	if err := validateMaterializationSelectorSyntax(target, selector); err != nil {
		return err
	}
	if selector == "$" {
		if filepath.Ext(target) == ".json" && !json.Valid(content) {
			return errors.New("JSON artifact has invalid syntax")
		}
		return nil
	}
	if filepath.Ext(target) != ".json" || !strings.HasPrefix(selector, "$.") || !json.Valid(content) {
		return fmt.Errorf("selector %q is unsupported", selector)
	}
	for _, field := range strings.Split(strings.TrimPrefix(selector, "$."), ".") {
		if field == "" {
			return fmt.Errorf("selector %q is unsupported", selector)
		}
	}
	return nil
}

func validateMaterializationSelectorSyntax(target, selector string) error {
	if selector == "$" {
		return nil
	}
	if filepath.Ext(target) != ".json" || !strings.HasPrefix(selector, "$.") {
		return fmt.Errorf("selector %q is unsupported", selector)
	}
	for _, field := range strings.Split(strings.TrimPrefix(selector, "$."), ".") {
		if field == "" {
			return fmt.Errorf("selector %q is unsupported", selector)
		}
	}
	return nil
}

func validateMaterializationTarget(target string) error {
	clean := path.Clean(target)
	unsafe := target == "" || target == "." || clean != target || path.IsAbs(target) ||
		clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(target, "\\\x00") ||
		strings.IndexFunc(target, unicode.IsControl) >= 0
	if unsafe {
		return fmt.Errorf("target %q escapes its State Component root", target)
	}
	return nil
}

func materializationRoot(batch ProfileMaterializationBatch, root MaterializationRoot) (string, error) {
	switch root {
	case MaterializationHome:
		return batch.HomeRoot, nil
	case MaterializationWorkspace:
		return batch.WorkspaceRoot, nil
	default:
		return "", fmt.Errorf("apply Profile Materializations: unsupported State Component root %q", root)
	}
}

func batchMaterializationRoot(
	batch ProfileMaterializationBatch,
	roots map[MaterializationRoot]*os.Root,
	stateComponent MaterializationRoot,
) (*os.Root, error) {
	if root := roots[stateComponent]; root != nil {
		return root, nil
	}
	rootPath, err := materializationRoot(batch, stateComponent)
	if err != nil {
		return nil, err
	}
	root, err := openMaterializationRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("apply Profile Materializations: %w", err)
	}
	roots[stateComponent] = root
	return root, nil
}

func openMaterializationRoot(rootPath string) (*os.Root, error) {
	if !filepath.IsAbs(rootPath) {
		return nil, errors.New("State Component root must be absolute")
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("inspect State Component root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("State Component root is not a safe directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open State Component root: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("State Component root changed while opening")
	}
	return root, nil
}

func writeMaterializedFile(root *os.Root, target string, content []byte, mode os.FileMode, create bool) error {
	directory := path.Dir(target)
	if directory != "." {
		if err := root.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	name, err := randomMaterializationName()
	if err != nil {
		return err
	}
	temporary := path.Join(directory, name)
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if create {
		return root.Link(temporary, target)
	}
	return root.Rename(temporary, target)
}

func observeMaterializedFile(root *os.Root, target, selector string) (string, error) {
	content, exists, err := readMaterializedFile(root, target)
	if err != nil || !exists {
		return "", err
	}
	if selector != "$" {
		content, exists, err = selectJSON(content, selector)
		if err != nil || !exists {
			return "", err
		}
	}
	return materializationContentDigest(content), nil
}

func readMaterializedFile(root *os.Root, target string) ([]byte, bool, error) {
	info, err := root.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("target is not a regular file")
	}
	file, err := root.Open(target)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, false, errors.New("target changed while opening")
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, false, err
	}
	current, err := root.Lstat(target)
	if err != nil || !os.SameFile(opened, current) {
		return nil, false, errors.New("target changed while reading")
	}
	return content, true, nil
}

func selectJSON(content []byte, selector string) ([]byte, bool, error) {
	var value any
	if err := decodeMaterializationJSON(content, &value); err != nil {
		return nil, false, fmt.Errorf("decode JSON target: %w", err)
	}
	for _, field := range strings.Split(strings.TrimPrefix(selector, "$."), ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("selector %q crosses a non-object value", selector)
		}
		value, ok = object[field]
		if !ok {
			return nil, false, nil
		}
	}
	selected, err := json.Marshal(value)
	return selected, true, err
}

func mergeJSONSelection(root *os.Root, target, selector string, desired []byte) ([]byte, error) {
	content, exists, err := readMaterializedFile(root, target)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if exists {
		if err := decodeMaterializationJSON(content, &document); err != nil {
			return nil, fmt.Errorf("decode JSON target: %w", err)
		}
	} else {
		document = make(map[string]any)
	}
	var value any
	if err := decodeMaterializationJSON(desired, &value); err != nil {
		return nil, fmt.Errorf("decode desired JSON value: %w", err)
	}
	fields := strings.Split(strings.TrimPrefix(selector, "$."), ".")
	current := document
	for _, field := range fields[:len(fields)-1] {
		next, exists := current[field]
		if !exists {
			child := make(map[string]any)
			current[field] = child
			current = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("selector %q crosses a non-object value", selector)
		}
		current = child
	}
	current[fields[len(fields)-1]] = value
	return json.Marshal(document)
}

func removeJSONSelection(root *os.Root, target, selector string) error {
	mode, err := materializedFileMode(root, target)
	if err != nil {
		return err
	}
	content, exists, err := readMaterializedFile(root, target)
	if err != nil || !exists {
		return err
	}
	var document map[string]any
	if err := decodeMaterializationJSON(content, &document); err != nil {
		return fmt.Errorf("decode JSON target: %w", err)
	}
	fields := strings.Split(strings.TrimPrefix(selector, "$."), ".")
	current := document
	for _, field := range fields[:len(fields)-1] {
		next, ok := current[field].(map[string]any)
		if !ok {
			return fmt.Errorf("selector %q crosses a non-object value", selector)
		}
		current = next
	}
	delete(current, fields[len(fields)-1])
	updated, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return writeMaterializedFile(root, target, updated, mode, false)
}

func materializedFileExists(root *os.Root, target string) (bool, error) {
	_, exists, err := readMaterializedFile(root, target)
	return exists, err
}

func decodeMaterializationJSON(content []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON target must contain one document")
	}
	return nil
}

func materializedFileMode(root *os.Root, target string) (os.FileMode, error) {
	info, err := root.Lstat(target)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("target is not a regular file")
	}
	return info.Mode().Perm(), nil
}

func materializationSelector(item ProfileMaterialization) string {
	if item.Artifact != nil {
		return item.Artifact.Selector
	}
	return item.Selector
}

func randomMaterializationName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return ".sshai-materialization-" + hex.EncodeToString(value[:]), nil
}

func materializationDigestState(value string) (profile.DigestState, error) {
	if value == "" {
		return profile.AbsentDigest(), nil
	}
	if !profileSHA256.MatchString(value) {
		return profile.DigestState{}, errors.New("materialization digest is invalid")
	}
	return profile.PresentDigest(value)
}

func materializationContentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func materializationResult(item ProfileMaterialization, operation profile.PlanOperation, lastApplied, observed string) ProfileMaterializationResult {
	adapter := materializationFileAdapter
	if item.Mode == MaterializationReferenced {
		adapter = materializationReferenceAdapter
	}
	selector := item.Selector
	desired := ""
	if item.Artifact != nil {
		selector = item.Artifact.Selector
		desired = item.Artifact.ContentDigest
	}
	return ProfileMaterializationResult{
		ID: item.ID, ArtifactID: item.ArtifactID, Mode: item.Mode,
		Adapter: adapter, AdapterVersion: materializationAdapterVersion, Root: item.Root, Target: item.Target,
		Selector: selector, DesiredDigest: desired,
		LastAppliedDigest: lastApplied, ObservedDigest: observed, Operation: operation,
		RequirementState: item.RequirementState,
	}
}

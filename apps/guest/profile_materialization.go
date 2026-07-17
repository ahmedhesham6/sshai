package guest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/ahmedhesham6/sshai/libs/adapters"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
	"github.com/pelletier/go-toml/v2"
)

const (
	materializationFileAdapter      = "file"
	materializationReferenceAdapter = "reference"
	materializationAdapterVersion   = "v1"
)

type ProfileMaterializationBatch struct {
	HomeRoot      string
	WorkspaceRoot string
	Intent        profile.PlanIntent
	Items         []ProfileMaterialization
	Approvals     map[string]ApprovalMarker
	Metrics       domain.Metrics
}

var profileSHA256 = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var ErrProfileMaterializationBlocked = errors.New("Profile Materialization is blocked")

type plannedProfileMaterialization struct {
	result    ProfileMaterializationResult
	root      *os.Root
	content   []byte
	mode      os.FileMode
	files     []MaterializationFile
	directory bool
}

type materializationBackup struct {
	root      *os.Root
	target    string
	selector  string
	directory bool
	existed   bool
	mode      os.FileMode
	content   []byte
	files     []MaterializationFile
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
	seenComponents := make(map[string]struct{}, len(batch.Items))
	ownedTargets := make(map[string][]string, len(batch.Items))
	plans := make([]plannedProfileMaterialization, 0, len(batch.Items))
	for _, item := range batch.Items {
		if item.ComponentID == "" {
			item.ComponentID = item.ID
		}
		if item.ID == "" {
			item.ID = item.ComponentID
		}
		if item.ID != item.ComponentID {
			return nil, fmt.Errorf("apply Profile Materializations: identity %q does not match Component ID %q", item.ID, item.ComponentID)
		}
		if required, reason := materializationApprovalPolicy(item); required {
			item.ApprovalRequired = true
			if item.ApprovalReason == "" {
				item.ApprovalReason = reason
			}
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, fmt.Errorf("apply Profile Materializations: duplicate identity %q", item.ID)
		}
		if _, duplicate := seenComponents[item.ComponentID]; duplicate {
			return nil, fmt.Errorf("apply Profile Materializations: duplicate Component ID %q", item.ComponentID)
		}
		seen[item.ID] = struct{}{}
		seenComponents[item.ComponentID] = struct{}{}
		if err := recordMaterializationOwnership(ownedTargets, item); err != nil {
			return nil, fmt.Errorf("apply Profile Materializations: %w", err)
		}
		plan, err := prepareProfileMaterialization(batch, roots, item)
		if err != nil {
			return nil, fmt.Errorf("apply Profile Materializations: plan %q: %w", item.ID, err)
		}
		if item.ApprovalRequired && !approvalGranted(batch.Approvals, item) && plan.result.Operation != profile.OperationOrphan && plan.result.Operation != profile.OperationSkip {
			plan.result.Operation = profile.OperationRequiresInput
		}
		plans = append(plans, plan)
	}
	results := materializationResults(plans)
	recordMaterializationConflictMetrics(batch.Metrics, results)
	for _, plan := range plans {
		if plan.blocked() {
			return results, fmt.Errorf("apply Profile Materializations: %w: %q requires %s resolution", ErrProfileMaterializationBlocked, plan.result.ID, plan.result.Operation)
		}
	}
	backups, err := snapshotMaterializationPlans(plans)
	if err != nil {
		return results, fmt.Errorf("apply Profile Materializations: prepare rollback: %w", err)
	}
	for index := range plans {
		if err := plans[index].apply(); err != nil {
			if rollbackErr := rollbackMaterializations(backups); rollbackErr != nil {
				return materializationResults(plans), fmt.Errorf("apply Profile Materializations: %w; rollback: %w", err, rollbackErr)
			}
			return materializationResults(plans), fmt.Errorf("apply Profile Materializations: %w", err)
		}
	}
	results = materializationResults(plans)
	if batch.Metrics != nil {
		for _, result := range results {
			if result.Operation != profile.OperationSkip {
				batch.Metrics.AddCounter(domain.MetricComponentMaterializationsTotal, 1)
			}
		}
	}
	return results, nil
}

func recordMaterializationConflictMetrics(metrics domain.Metrics, results []ProfileMaterializationResult) {
	if metrics == nil {
		return
	}
	for _, result := range results {
		if result.Operation == profile.OperationConflict {
			metrics.AddCounter(domain.MetricComponentConflictsTotal, 1)
		}
	}
}

func snapshotMaterializationPlans(plans []plannedProfileMaterialization) ([]materializationBackup, error) {
	backups := make([]materializationBackup, 0, len(plans))
	for _, plan := range plans {
		if plan.result.Operation == profile.OperationSkip || plan.result.Operation == profile.OperationOrphan || plan.result.Adapter == materializationReferenceAdapter {
			continue
		}
		backup, err := snapshotMaterialization(plan.root, plan.result.Target, plan.result.Selector, plan.directory)
		if err != nil {
			return nil, fmt.Errorf("snapshot %q: %w", plan.result.ID, err)
		}
		backups = append(backups, backup)
	}
	return backups, nil
}

func snapshotMaterialization(root *os.Root, target, selector string, directory bool) (materializationBackup, error) {
	backup := materializationBackup{root: root, target: target, selector: selector, directory: directory}
	info, err := root.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return backup, nil
	}
	if err != nil {
		return materializationBackup{}, err
	}
	backup.existed = true
	backup.mode = info.Mode().Perm()
	if directory {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return materializationBackup{}, errors.New("directory target is not a real directory")
		}
		files := make([]MaterializationFile, 0)
		err = fs.WalkDir(root.FS(), target, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if name == target || entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return errors.New("directory target contains an unsafe entry")
			}
			content, readErr := root.ReadFile(name)
			if readErr != nil {
				return readErr
			}
			fileInfo, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			files = append(files, MaterializationFile{Path: strings.TrimPrefix(name, target+"/"), Content: content, Mode: fileInfo.Mode().Perm()})
			return nil
		})
		if err != nil {
			return materializationBackup{}, err
		}
		backup.files = files
		return backup, nil
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return materializationBackup{}, errors.New("target is not a regular file")
	}
	backup.content, err = root.ReadFile(target)
	return backup, err
}

func rollbackMaterializations(backups []materializationBackup) error {
	var rollbackErr error
	for index := len(backups) - 1; index >= 0; index-- {
		if err := backups[index].restore(); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	return rollbackErr
}

func (backup materializationBackup) restore() error {
	if err := removeMaterializationTarget(backup.root, backup.target); err != nil {
		return fmt.Errorf("remove current %q: %w", backup.target, err)
	}
	if !backup.existed {
		return nil
	}
	if backup.directory {
		if err := backup.root.MkdirAll(backup.target, backup.mode); err != nil {
			return fmt.Errorf("restore directory %q: %w", backup.target, err)
		}
		if err := backup.root.Chmod(backup.target, backup.mode); err != nil {
			return fmt.Errorf("restore directory mode %q: %w", backup.target, err)
		}
		if len(backup.files) == 0 {
			return nil
		}
		if err := writeMaterializedDirectory(backup.root, backup.target, backup.files); err != nil {
			return fmt.Errorf("restore directory contents %q: %w", backup.target, err)
		}
		return backup.root.Chmod(backup.target, backup.mode)
	}
	if err := writeMaterializedFile(backup.root, backup.target, backup.content, backup.mode, true); err != nil {
		return fmt.Errorf("restore file %q: %w", backup.target, err)
	}
	return nil
}

func removeMaterializationTarget(root *os.Root, target string) error {
	info, err := root.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
		return root.RemoveAll(target)
	}
	return root.Remove(target)
}

func materializationApprovalPolicy(item ProfileMaterialization) (bool, string) {
	switch item.Kind {
	case domain.ComponentHook, domain.ComponentExtension:
		return true, "hook and extension Components require explicit consent"
	case domain.ComponentPermissionPolicy:
		return true, "permission-policy Component requires explicit consent"
	case domain.ComponentIntegration:
		return true, "integration Component requires explicit consent"
	}
	if item.TrustClass == domain.TrustPermission {
		return true, "permission Component requires explicit consent"
	}
	return false, ""
}

func approvalGranted(markers map[string]ApprovalMarker, item ProfileMaterialization) bool {
	marker, ok := markers[item.ComponentID]
	if !ok || marker.ComponentID != item.ComponentID || marker.ComponentDigest != item.ComponentDigest {
		return false
	}
	if marker.LockID != "" && marker.LockID != item.LockID {
		return false
	}
	if marker.LockDigest != "" && marker.LockDigest != item.LockDigest {
		return false
	}
	return marker.LockID != "" || marker.LockDigest != ""
}

func prepareProfileMaterialization(batch ProfileMaterializationBatch, roots map[MaterializationRoot]*os.Root, item ProfileMaterialization) (plannedProfileMaterialization, error) {
	if item.ContentDigest == "" {
		operation, observed, err := planRemovedMaterialization(batch, roots, item)
		return plannedProfileMaterialization{result: materializationResult(item, operation, item.LastAppliedDigest, observed), root: roots[item.Root], directory: materializationIsDirectory(item)}, err
	}
	if item.Mode == MaterializationReferenced {
		operation, err := planReferencedMaterialization(item, batch.Intent)
		return plannedProfileMaterialization{result: materializationResult(item, operation, item.LastAppliedDigest, item.ObservedDigest)}, err
	}
	if item.Mode != MaterializationManaged && item.Mode != MaterializationSeeded {
		return plannedProfileMaterialization{}, fmt.Errorf("unsupported mode %q", item.Mode)
	}
	if item.Scope == domain.ScopeProject && item.Mode != MaterializationSeeded {
		return plannedProfileMaterialization{}, errors.New("project-scope materialization must be seeded")
	}
	if err := validateDesiredMaterialization(item); err != nil {
		return plannedProfileMaterialization{}, err
	}
	if selector := item.Selector; filepathExt(item.Target) == ".toml" && selector != "" && selector != "$" {
		canonical, err := canonicalTOMLDesiredSelection(item.Content, selector)
		if err != nil {
			return plannedProfileMaterialization{}, fmt.Errorf("canonicalize desired TOML selection: %w", err)
		}
		item.ContentDigest = materializationContentDigest(canonical)
	}
	root, err := batchMaterializationRoot(batch, roots, item.Root)
	if err != nil {
		return plannedProfileMaterialization{}, err
	}
	selector := item.Selector
	if selector == "" {
		selector = "$"
	}
	directory := materializationIsDirectory(item)
	observed, err := observeMaterialization(root, item.Target, selector, directory)
	if err != nil {
		return plannedProfileMaterialization{}, fmt.Errorf("observe: %w", err)
	}
	if observed != item.ObservedDigest {
		return plannedProfileMaterialization{}, fmt.Errorf("%w: observation changed", ErrProfileMaterializationBlocked)
	}
	operation, err := planFileMaterialization(item, observed, batch.Intent)
	if err == nil && operation == profile.OperationSkip && item.EffectiveCacheKeyChanged {
		operation = profile.OperationUpdate
	}
	return plannedProfileMaterialization{
		result: materializationResult(item, operation, item.LastAppliedDigest, observed), root: root,
		content: append([]byte(nil), item.Content...), mode: materializationMode(item),
		files: cloneMaterializationFiles(item.Files), directory: directory,
	}, err
}

func materializationMode(item ProfileMaterialization) os.FileMode {
	if !materializationIsDirectory(item) {
		if item.FileMode != 0 {
			return item.FileMode
		}
		return 0o600
	}
	return 0o700
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
	observed, err := observeMaterialization(plan.root, plan.result.Target, plan.result.Selector, plan.directory)
	if err != nil || observed != plan.result.ObservedDigest {
		return fmt.Errorf("%w: observation for %q changed before mutation", ErrProfileMaterializationBlocked, plan.result.ID)
	}
	if operation == profile.OperationRemove {
		if plan.directory {
			err = plan.root.RemoveAll(plan.result.Target)
		} else if plan.result.Selector == "$" {
			err = plan.root.Remove(plan.result.Target)
		} else if filepathExt(plan.result.Target) == ".toml" {
			err = removeTOMLSelection(plan.root, plan.result.Target, plan.result.Selector)
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
	if plan.directory {
		if err := writeMaterializedDirectory(plan.root, plan.result.Target, plan.files); err != nil {
			return fmt.Errorf("%s %q: %w", operation, plan.result.ID, err)
		}
	} else {
		content := plan.content
		if plan.result.Selector != "$" {
			if filepathExt(plan.result.Target) == ".toml" {
				content, err = mergeTOMLSelection(plan.root, plan.result.Target, plan.result.Selector, content)
			} else {
				content, err = mergeJSONSelection(plan.root, plan.result.Target, plan.result.Selector, content)
			}
			if err != nil {
				return fmt.Errorf("merge %q: %w", plan.result.ID, err)
			}
		}
		current, exists, err := readMaterializedFile(plan.root, plan.result.Target)
		if err != nil {
			return fmt.Errorf("inspect %q before mutation: %w", plan.result.ID, err)
		}
		if !exists || !bytes.Equal(current, content) {
			if err := writeMaterializedFile(plan.root, plan.result.Target, content, plan.mode, !exists); err != nil {
				return fmt.Errorf("%s %q: %w", operation, plan.result.ID, err)
			}
		} else {
			currentMode, err := materializedFileMode(plan.root, plan.result.Target)
			if err != nil {
				return fmt.Errorf("inspect mode %q before mutation: %w", plan.result.ID, err)
			}
			if currentMode.Perm() != plan.mode.Perm() {
				if err := plan.root.Chmod(plan.result.Target, plan.mode); err != nil {
					return fmt.Errorf("chmod %q: %w", plan.result.ID, err)
				}
			}
		}
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
	selector := item.Selector
	if selector == "" || materializationIsDirectory(item) {
		selector = "$"
	}
	key := string(item.Root) + "\x00" + item.Target
	for _, existing := range owned[key] {
		if adapters.MaterializationSelectorsOverlap(selector, existing) {
			return fmt.Errorf("target %q has overlapping selectors %q and %q", item.Target, existing, selector)
		}
	}
	owned[key] = append(owned[key], selector)
	return nil
}

func planRemovedMaterialization(batch ProfileMaterializationBatch, roots map[MaterializationRoot]*os.Root, item ProfileMaterialization) (profile.PlanOperation, string, error) {
	if err := validateRecordedMaterialization(item); err != nil {
		return "", "", err
	}
	lastApplied, err := materializationDigestState(item.LastAppliedDigest)
	if err != nil {
		return "", "", err
	}
	observed := item.ObservedDigest
	directory := materializationIsDirectory(item)
	if item.Mode != MaterializationReferenced {
		root, err := batchMaterializationRoot(batch, roots, item.Root)
		if err != nil {
			return "", "", err
		}
		observed, err = observeMaterialization(root, item.Target, item.Selector, directory)
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
	if strings.TrimSpace(item.ID) == "" {
		return errors.New("materialization identity is required")
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
	desired, err := materializationDigestState(item.ContentDigest)
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
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ComponentID) == "" {
		return errors.New("materialization identity is required")
	}
	if len(item.Content) != 0 || len(item.Files) != 0 || item.ContentSize != 0 || item.LastAppliedDigest != "" {
		return fmt.Errorf("referenced materialization %q must contain metadata only", item.ID)
	}
	if !profileSHA256.MatchString(item.ContentDigest) {
		return fmt.Errorf("referenced materialization %q digest is invalid", item.ID)
	}
	if err := validateMaterializationTarget(item.Target); err != nil {
		return fmt.Errorf("referenced materialization %q: %w", item.ID, err)
	}
	return nil
}

func planFileMaterialization(item ProfileMaterialization, observed string, intent profile.PlanIntent) (profile.PlanOperation, error) {
	desired, err := materializationDigestState(item.ContentDigest)
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
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ComponentID) == "" {
		return errors.New("materialization identity is required")
	}
	if !item.Kind.Valid() {
		return fmt.Errorf("materialization %q has invalid Component type %q", item.ID, item.Kind)
	}
	if item.ContentSize < 0 {
		return fmt.Errorf("materialization %q content size is invalid", item.ID)
	}
	if item.FileMode&^0o777 != 0 {
		return fmt.Errorf("materialization %q file mode is invalid", item.ID)
	}
	if !materializationIsDirectory(item) {
		if int64(len(item.Content)) != item.ContentSize {
			return fmt.Errorf("materialization %q content size mismatch", item.ID)
		}
		if !profileSHA256.MatchString(item.ContentDigest) || materializationContentDigest(item.Content) != item.ContentDigest {
			return fmt.Errorf("materialization %q content digest mismatch", item.ID)
		}
	} else {
		if len(item.Files) == 0 {
			return fmt.Errorf("materialization %q directory plan has no files", item.ID)
		}
		if item.Selector != "" && item.Selector != "$" {
			return fmt.Errorf("materialization %q directory plan cannot have a selector", item.ID)
		}
		if got := directoryMaterializationDigest(item.Files); !profileSHA256.MatchString(item.ContentDigest) || got != item.ContentDigest {
			return fmt.Errorf("materialization %q directory digest mismatch", item.ID)
		}
		for _, file := range item.Files {
			if err := validateMaterializationRelativePath(file.Path); err != nil {
				return fmt.Errorf("materialization %q: %w", item.ID, err)
			}
			if file.Mode.Perm() != 0o644 && file.Mode.Perm() != 0o755 {
				return fmt.Errorf("materialization %q file %q mode is invalid", item.ID, file.Path)
			}
		}
	}
	if err := validateMaterializationTarget(item.Target); err != nil {
		return fmt.Errorf("materialization %q: %w", item.ID, err)
	}
	selector := item.Selector
	if selector == "" {
		selector = "$"
	}
	if err := validateMaterializationSelector(item.Target, selector, item.Content); err != nil && !materializationIsDirectory(item) {
		return fmt.Errorf("materialization %q: %w", item.ID, err)
	}
	return nil
}

func validateMaterializationSelector(target, selector string, content []byte) error {
	if err := validateMaterializationSelectorSyntax(target, selector); err != nil {
		return err
	}
	if selector == "$" {
		switch filepathExt(target) {
		case ".json":
			if !json.Valid(content) {
				return errors.New("JSON component has invalid syntax")
			}
		case ".toml":
			if _, err := decodeMaterializationTOML(content); err != nil {
				return fmt.Errorf("TOML component has invalid syntax: %w", err)
			}
		}
		return nil
	}
	if filepathExt(target) == ".json" {
		if !json.Valid(content) {
			return fmt.Errorf("selector %q is unsupported", selector)
		}
		return nil
	}
	if filepathExt(target) != ".toml" {
		return fmt.Errorf("selector %q is unsupported", selector)
	}
	if _, err := decodeTOMLSelection(content, selector); err != nil {
		return fmt.Errorf("selector %q is unsupported: %w", selector, err)
	}
	return nil
}

func validateMaterializationSelectorSyntax(target, selector string) error {
	if selector == "" || selector == "$" {
		return nil
	}
	if (filepathExt(target) != ".json" && filepathExt(target) != ".toml") || !strings.HasPrefix(selector, "$.") {
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
	unsafe := target == "" || target == "." || clean != target || path.IsAbs(target) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(target, "\\\x00") || strings.IndexFunc(target, unicode.IsControl) >= 0
	if unsafe {
		return fmt.Errorf("target %q escapes its State Component root", target)
	}
	return nil
}

func validateMaterializationRelativePath(name string) error {
	clean := path.Clean(name)
	if name == "" || clean != name || path.IsAbs(name) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(name, "\\\x00") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("file path %q escapes its native target", name)
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

func batchMaterializationRoot(batch ProfileMaterializationBatch, roots map[MaterializationRoot]*os.Root, stateComponent MaterializationRoot) (*os.Root, error) {
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
	if !strings.HasPrefix(rootPath, "/") || path.Clean(rootPath) != rootPath {
		return nil, errors.New("State Component root must be an absolute clean path")
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
	temporary, err := randomMaterializationName()
	if err != nil {
		return err
	}
	temporary = path.Join(directory, temporary)
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

func writeMaterializedDirectory(root *os.Root, target string, files []MaterializationFile) error {
	parent := path.Dir(target)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	temporary, err := randomMaterializationName()
	if err != nil {
		return err
	}
	temporary = path.Join(parent, temporary)
	if err := root.Mkdir(temporary, 0o700); err != nil {
		return err
	}
	defer root.RemoveAll(temporary)
	for _, file := range files {
		if err := writeMaterializedFile(root, path.Join(temporary, file.Path), file.Content, file.Mode, true); err != nil {
			return err
		}
	}
	info, err := root.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return root.Rename(temporary, target)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("directory target is not a safe directory")
	}
	backup, err := randomMaterializationName()
	if err != nil {
		return err
	}
	backup = path.Join(parent, backup)
	if err := root.Rename(target, backup); err != nil {
		return err
	}
	if err := root.Rename(temporary, target); err != nil {
		_ = root.Rename(backup, target)
		return err
	}
	return root.RemoveAll(backup)
}

func observeMaterialization(root *os.Root, target, selector string, directory bool) (string, error) {
	if directory {
		return observeMaterializedDirectory(root, target)
	}
	return observeMaterializedFile(root, target, selector)
}

func observeMaterializedFile(root *os.Root, target, selector string) (string, error) {
	content, exists, err := readMaterializedFile(root, target)
	if err != nil || !exists {
		return "", err
	}
	if selector != "" && selector != "$" {
		if filepathExt(target) == ".toml" {
			content, exists, err = selectTOML(content, selector)
		} else {
			content, exists, err = selectJSON(content, selector)
		}
		if err != nil || !exists {
			return "", err
		}
	}
	return materializationContentDigest(content), nil
}

func observeMaterializedDirectory(root *os.Root, target string) (string, error) {
	info, err := root.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("directory target is not a real directory")
	}
	files := make([]MaterializationFile, 0)
	err = fs.WalkDir(root.FS(), target, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == target || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("directory target contains an unsafe entry")
		}
		relative := strings.TrimPrefix(name, target+"/")
		content, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, MaterializationFile{Path: relative, Content: content, Mode: fileInfo.Mode().Perm()})
		return nil
	})
	if err != nil {
		return "", err
	}
	return directoryMaterializationDigest(files), nil
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

func selectTOML(content []byte, selector string) ([]byte, bool, error) {
	document, err := decodeMaterializationTOML(content)
	if err != nil {
		return nil, false, fmt.Errorf("decode TOML target: %w", err)
	}
	value, exists, err := lookupTOMLSelection(document, selector)
	if err != nil || !exists {
		return nil, exists, err
	}
	selected, err := canonicalTOMLSelection(value, selector)
	return selected, true, err
}

func mergeTOMLSelection(root *os.Root, target, selector string, desired []byte) ([]byte, error) {
	content, exists, err := readMaterializedFile(root, target)
	if err != nil {
		return nil, err
	}
	document := make(map[string]any)
	if exists {
		document, err = decodeMaterializationTOML(content)
		if err != nil {
			return nil, fmt.Errorf("decode TOML target: %w", err)
		}
	}
	value, err := decodeTOMLSelection(desired, selector)
	if err != nil {
		return nil, fmt.Errorf("decode desired TOML value: %w", err)
	}
	if exists {
		currentValue, currentExists, err := lookupTOMLSelection(document, selector)
		if err != nil {
			return nil, err
		}
		if currentExists {
			currentCanonical, err := canonicalTOMLSelection(currentValue, selector)
			if err != nil {
				return nil, err
			}
			desiredCanonical, err := canonicalTOMLSelection(value, selector)
			if err != nil {
				return nil, err
			}
			if bytes.Equal(currentCanonical, desiredCanonical) {
				return content, nil
			}
		}
	}
	fields := strings.Split(strings.TrimPrefix(selector, "$."), ".")
	current := document
	for _, field := range fields[:len(fields)-1] {
		next, present := current[field]
		if !present {
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
	return toml.Marshal(document)
}

func removeTOMLSelection(root *os.Root, target, selector string) error {
	mode, err := materializedFileMode(root, target)
	if err != nil {
		return err
	}
	content, exists, err := readMaterializedFile(root, target)
	if err != nil || !exists {
		return err
	}
	document, err := decodeMaterializationTOML(content)
	if err != nil {
		return fmt.Errorf("decode TOML target: %w", err)
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
	updated, err := toml.Marshal(document)
	if err != nil {
		return err
	}
	return writeMaterializedFile(root, target, updated, mode, false)
}

func decodeMaterializationTOML(content []byte) (map[string]any, error) {
	var document map[string]any
	if err := toml.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, errors.New("TOML target must contain a document")
	}
	return document, nil
}

func decodeTOMLSelection(content []byte, selector string) (any, error) {
	document, err := decodeMaterializationTOML(content)
	if err != nil {
		return nil, err
	}
	if value, exists, lookupErr := lookupTOMLSelection(document, selector); lookupErr != nil {
		return nil, lookupErr
	} else if exists {
		return value, nil
	}
	if len(document) == 1 {
		if value, exists := document["value"]; exists {
			return value, nil
		}
	}
	fields := strings.Split(strings.TrimPrefix(selector, "$."), ".")
	if value, exists := document[fields[len(fields)-1]]; exists {
		return value, nil
	}
	return document, nil
}

func lookupTOMLSelection(document map[string]any, selector string) (any, bool, error) {
	fields := strings.Split(strings.TrimPrefix(selector, "$."), ".")
	var value any = document
	for _, field := range fields {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("selector %q crosses a non-object value", selector)
		}
		value, ok = object[field]
		if !ok {
			return nil, false, nil
		}
	}
	return value, true, nil
}

func canonicalTOMLSelection(value any, selector string) ([]byte, error) {
	if object, ok := value.(map[string]any); ok {
		return toml.Marshal(object)
	}
	fields := strings.Split(strings.TrimPrefix(selector, "$."), ".")
	return toml.Marshal(map[string]any{fields[len(fields)-1]: value})
}

func canonicalTOMLDesiredSelection(content []byte, selector string) ([]byte, error) {
	value, err := decodeTOMLSelection(content, selector)
	if err != nil {
		return nil, err
	}
	return canonicalTOMLSelection(value, selector)
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

func materializationIsDirectory(item ProfileMaterialization) bool {
	return item.Directory || len(item.Files) > 0
}

func materializationResult(item ProfileMaterialization, operation profile.PlanOperation, lastApplied, observed string) ProfileMaterializationResult {
	adapter := item.AdapterID
	if adapter == "" {
		adapter = materializationFileAdapter
	}
	adapterVersion := item.AdapterVersion
	if adapterVersion == "" {
		adapterVersion = materializationAdapterVersion
	}
	if item.Mode == MaterializationReferenced {
		adapter = materializationReferenceAdapter
	}
	selector := item.Selector
	if selector == "" {
		selector = "$"
	}
	return ProfileMaterializationResult{
		ID: item.ID, LockID: item.LockID, LockDigest: item.LockDigest, CapsuleDigest: item.CapsuleDigest,
		ComponentID: item.ComponentID, ComponentDigest: item.ComponentDigest,
		Adapter: adapter, AdapterID: adapter, AdapterVersion: adapterVersion, TargetAgentVersion: item.TargetAgentVersion,
		Scope: item.Scope, NonSecretOverridesDigest: item.NonSecretOverridesDigest,
		SecretVersionIdentifiers: append([]string(nil), item.SecretVersionIdentifiers...), EffectiveCacheKey: item.EffectiveCacheKey,
		Mode: item.Mode, Root: item.Root, Target: item.Target, Selector: selector,
		Directory: materializationIsDirectory(item), FilePaths: append([]string(nil), item.FilePaths...),
		DesiredDigest: item.ContentDigest, LastAppliedDigest: lastApplied, ObservedDigest: observed,
		Operation: operation, RequirementState: item.RequirementState,
		ApprovalRequired: item.ApprovalRequired, ApprovalReason: item.ApprovalReason,
		CredentialRequirementDigest: item.CredentialRequirementDigest,
	}
}

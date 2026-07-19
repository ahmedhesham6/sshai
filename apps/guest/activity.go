// Package guest observes bounded Runtime activity without retaining process,
// connection, or container details in Activity Snapshots.
package guest

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

type ProcessState string

const (
	ProcessRunning ProcessState = "running"
	ProcessWaiting ProcessState = "waiting"
)

type ContainerState string

const (
	ContainerRunning ContainerState = "running"
	ContainerStopped ContainerState = "stopped"
)

// ExternalSample is the bounded input expected from an operating-system
// adapter. It deliberately has no command line, environment, payload, secret,
// or CPU fields.
type ExternalSample struct {
	ObservedAt           time.Time
	GuestSequence        int64
	UserUID              int
	ActiveSSHConnections []ActiveSSHConnectionSample
	Processes            []ProcessSample
	Containers           []ContainerSample
}

type ActiveSSHConnectionSample struct {
	ID string
}

type ProcessSample struct {
	PID         int
	ParentPID   int
	OwnerUID    int
	Executable  string
	State       ProcessState
	CgroupPath  string
	ContainerID string
}

type ContainerSample struct {
	ID        string
	Selection string
	State     ContainerState
}

// SampleSource is the seam implemented by future Linux process, sshd, and
// container-engine adapters.
type SampleSource interface {
	Sample(context.Context) (ExternalSample, error)
}

// Allowlists are exact identities used to classify sampled activity.
type Allowlists struct {
	CodexExecutables     []string
	ClaudeExecutables    []string
	ProtectedExecutables []string
	SelectedContainers   []string
}

type Observer struct {
	runtimeID string
	source    SampleSource
	codex     map[string]struct{}
	claude    map[string]struct{}
	protected map[string]struct{}
	selected  map[string]struct{}
}

func NewObserver(runtimeID string, source SampleSource, allowlists Allowlists) (*Observer, error) {
	if runtimeID == "" {
		return nil, errors.New("activity observer runtime ID is required")
	}
	if source == nil {
		return nil, errors.New("activity observer sample source is required")
	}
	occupied := make(map[string]string)
	codex, err := processIdentitySet("Codex", allowlists.CodexExecutables, occupied)
	if err != nil {
		return nil, err
	}
	claude, err := processIdentitySet("Claude", allowlists.ClaudeExecutables, occupied)
	if err != nil {
		return nil, err
	}
	protected, err := processIdentitySet("protected", allowlists.ProtectedExecutables, occupied)
	if err != nil {
		return nil, err
	}
	selected, err := identitySet("selected container", allowlists.SelectedContainers)
	if err != nil {
		return nil, err
	}
	return &Observer{
		runtimeID: runtimeID,
		source:    source,
		codex:     codex,
		claude:    claude,
		protected: protected,
		selected:  selected,
	}, nil
}

func processIdentitySet(category string, identities []string, occupied map[string]string) (map[string]struct{}, error) {
	set, err := identitySet(category+" executable", identities)
	if err != nil {
		return nil, err
	}
	for identity := range set {
		if existing, duplicate := occupied[identity]; duplicate {
			return nil, fmt.Errorf("process executable %q is classified as both %s and %s", identity, existing, category)
		}
		occupied[identity] = category
	}
	return set, nil
}

func identitySet(kind string, identities []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if identity == "" {
			return nil, fmt.Errorf("%s identity is empty", kind)
		}
		set[identity] = struct{}{}
	}
	return set, nil
}

func (observer *Observer) Observe(ctx context.Context) (ActivitySnapshot, error) {
	if observer == nil || observer.source == nil {
		return ActivitySnapshot{}, errors.New("activity observer is not initialized")
	}
	sample, err := observer.source.Sample(ctx)
	if err != nil {
		return ActivitySnapshot{}, fmt.Errorf("sample Runtime activity: %w", err)
	}
	if sample.ObservedAt.IsZero() || sample.GuestSequence <= 0 || sample.UserUID <= 0 {
		return ActivitySnapshot{}, errors.New("activity sample timestamp, positive guest sequence, and positive user UID are required")
	}

	connections, err := uniqueConnections(sample.ActiveSSHConnections)
	if err != nil {
		return ActivitySnapshot{}, err
	}
	processes, err := uniqueProcesses(sample.Processes)
	if err != nil {
		return ActivitySnapshot{}, err
	}
	containers, err := uniqueContainers(sample.Containers)
	if err != nil {
		return ActivitySnapshot{}, err
	}
	selectedContainerIDs := runningSelectedContainerIDs(containers, observer.selected)
	userSessionProcesses, escapedUserProcesses := countCgroupScopes(processes, sample.UserUID)

	return ActivitySnapshot{
		runtimeID:          observer.runtimeID,
		observedAt:         sample.ObservedAt.Round(0).UTC(),
		guestSequence:      sample.GuestSequence,
		sshConnections:     len(connections),
		codexProcesses:     countAgentRoots(processes, sample.UserUID, observer.codex),
		claudeProcesses:    countAgentRoots(processes, sample.UserUID, observer.claude),
		protectedProcesses: countProtectedProcesses(processes, sample.UserUID, observer.protected),
		selectedContainers: countContainers(containers, observer.selected),
		unknownUserProcesses: countUnknownUserProcesses(
			processes,
			sample.UserUID,
			observer.codex,
			observer.claude,
			observer.protected,
			selectedContainerIDs,
		),
		userSessionProcesses: userSessionProcesses,
		escapedUserProcesses: escapedUserProcesses,
	}, nil
}

func uniqueConnections(samples []ActiveSSHConnectionSample) (map[string]struct{}, error) {
	connections := make(map[string]struct{}, len(samples))
	for _, sample := range samples {
		if sample.ID == "" {
			return nil, errors.New("SSH connection sample identity is required")
		}
		connections[sample.ID] = struct{}{}
	}
	return connections, nil
}

func uniqueProcesses(samples []ProcessSample) (map[int]ProcessSample, error) {
	processes := make(map[int]ProcessSample, len(samples))
	for _, process := range samples {
		if process.PID <= 0 || process.ParentPID < 0 || process.OwnerUID < 0 || process.Executable == "" {
			return nil, errors.New("process sample requires a positive PID, parent PID, owner UID, and executable identity")
		}
		if process.State != ProcessRunning && process.State != ProcessWaiting {
			return nil, fmt.Errorf("process %d has invalid live state %q", process.PID, process.State)
		}
		if process.CgroupPath == "" || !strings.HasPrefix(process.CgroupPath, "/") || path.Clean(process.CgroupPath) != process.CgroupPath {
			return nil, fmt.Errorf("process %d has invalid cgroup v2 path %q", process.PID, process.CgroupPath)
		}
		if existing, duplicate := processes[process.PID]; duplicate && existing != process {
			return nil, fmt.Errorf("conflicting activity samples for process %d", process.PID)
		}
		processes[process.PID] = process
	}
	if err := validateProcessAncestry(processes); err != nil {
		return nil, err
	}
	return processes, nil
}

func validateProcessAncestry(processes map[int]ProcessSample) error {
	validated := make(map[int]struct{}, len(processes))
	for pid := range processes {
		path := make(map[int]struct{})
		for current := pid; ; {
			if _, ok := validated[current]; ok {
				break
			}
			if _, cycle := path[current]; cycle {
				return fmt.Errorf("process ancestry contains a cycle at process %d", current)
			}
			process, sampled := processes[current]
			if !sampled {
				break
			}
			path[current] = struct{}{}
			current = process.ParentPID
		}
		for current := range path {
			validated[current] = struct{}{}
		}
	}
	return nil
}

func uniqueContainers(samples []ContainerSample) (map[string]ContainerSample, error) {
	containers := make(map[string]ContainerSample, len(samples))
	for _, sample := range samples {
		if sample.ID == "" || sample.Selection == "" {
			return nil, errors.New("container sample identity and selection are required")
		}
		if sample.State != ContainerRunning && sample.State != ContainerStopped {
			return nil, fmt.Errorf("container %q has invalid state %q", sample.ID, sample.State)
		}
		if existing, duplicate := containers[sample.ID]; duplicate && existing != sample {
			return nil, fmt.Errorf("conflicting activity samples for container %q", sample.ID)
		}
		containers[sample.ID] = sample
	}
	return containers, nil
}

func countAgentRoots(processes map[int]ProcessSample, userUID int, allowlist map[string]struct{}) int {
	count := 0
	for _, process := range processes {
		if !liveTrackedAllowedProcess(process, userUID, allowlist) || hasTrackedAllowedAncestor(process, processes, userUID, allowlist) {
			continue
		}
		count++
	}
	return count
}

func hasTrackedAllowedAncestor(process ProcessSample, processes map[int]ProcessSample, userUID int, allowlist map[string]struct{}) bool {
	for parent, ok := processes[process.ParentPID]; ok; parent, ok = processes[parent.ParentPID] {
		if liveTrackedAllowedProcess(parent, userUID, allowlist) {
			return true
		}
	}
	return false
}

func countProtectedProcesses(processes map[int]ProcessSample, userUID int, allowlist map[string]struct{}) int {
	count := 0
	for _, process := range processes {
		if liveAllowedProcess(process, allowlist) && (process.OwnerUID != userUID || inUserSessionCgroup(process, userUID)) {
			count++
		}
	}
	return count
}

func liveAllowedProcess(process ProcessSample, allowlist map[string]struct{}) bool {
	_, allowed := allowlist[process.Executable]
	return allowed && (process.State == ProcessRunning || process.State == ProcessWaiting)
}

func liveTrackedAllowedProcess(process ProcessSample, userUID int, allowlist map[string]struct{}) bool {
	return inUserSessionCgroup(process, userUID) && liveAllowedProcess(process, allowlist)
}

func countUnknownUserProcesses(
	processes map[int]ProcessSample,
	userUID int,
	codex map[string]struct{},
	claude map[string]struct{},
	protected map[string]struct{},
	selectedContainerIDs map[string]struct{},
) int {
	count := 0
	for _, process := range processes {
		if isBaselineProcess(process, userUID) {
			continue
		}
		if !inUserSessionCgroup(process, userUID) {
			count++
			continue
		}
		if _, selected := selectedContainerIDs[process.ContainerID]; process.ContainerID != "" && selected {
			continue
		}
		if belongsToAgentTree(process, processes, userUID, codex) || belongsToAgentTree(process, processes, userUID, claude) {
			continue
		}
		if liveAllowedProcess(process, protected) {
			continue
		}
		count++
	}
	return count
}

func isBaselineProcess(process ProcessSample, userUID int) bool {
	if !inUserSessionCgroup(process, userUID) {
		return process.OwnerUID != userUID
	}

	// Cgroup membership attributes processes in the user session subtree to the
	// user regardless of owner UID. The user manager is the sole baseline
	// exception there, and only at its exact systemd identity and init scope.
	// Misclassifying an unknown process as baseline could auto-stop a busy
	// Runtime, so executable-only or cgroup-prefix exceptions are intentionally
	// forbidden here.
	userManagerCgroup := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/init.scope", userUID, userUID)
	if process.CgroupPath != userManagerCgroup {
		return false
	}
	return process.Executable == "/usr/lib/systemd/systemd" || process.Executable == "/lib/systemd/systemd"
}

func belongsToAgentTree(process ProcessSample, processes map[int]ProcessSample, userUID int, allowlist map[string]struct{}) bool {
	for current, ok := process, true; ok; current, ok = processes[current.ParentPID] {
		if liveTrackedAllowedProcess(current, userUID, allowlist) {
			return inCgroupSubtree(process.CgroupPath, current.CgroupPath)
		}
	}
	return false
}

func inCgroupSubtree(processPath, rootPath string) bool {
	return processPath == rootPath || strings.HasPrefix(processPath, rootPath+"/")
}

func countCgroupScopes(processes map[int]ProcessSample, userUID int) (userSession int, escaped int) {
	for _, process := range processes {
		if isBaselineProcess(process, userUID) {
			continue
		}
		if inUserSessionCgroup(process, userUID) {
			userSession++
		} else {
			escaped++
		}
	}
	return userSession, escaped
}

func inUserSessionCgroup(process ProcessSample, userUID int) bool {
	userSlice := fmt.Sprintf("/user.slice/user-%d.slice", userUID)
	return process.CgroupPath == userSlice || strings.HasPrefix(process.CgroupPath, userSlice+"/")
}

func runningSelectedContainerIDs(samples map[string]ContainerSample, allowlist map[string]struct{}) map[string]struct{} {
	selected := make(map[string]struct{})
	for _, sample := range samples {
		if _, allowed := allowlist[sample.Selection]; allowed && sample.State == ContainerRunning {
			selected[sample.ID] = struct{}{}
		}
	}
	return selected
}

func countContainers(samples map[string]ContainerSample, allowlist map[string]struct{}) int {
	count := 0
	for _, sample := range samples {
		if _, selected := allowlist[sample.Selection]; selected && sample.State == ContainerRunning {
			count++
		}
	}
	return count
}

type ActivitySnapshot struct {
	runtimeID            string
	observedAt           time.Time
	guestSequence        int64
	sshConnections       int
	codexProcesses       int
	claudeProcesses      int
	protectedProcesses   int
	selectedContainers   int
	unknownUserProcesses int
	userSessionProcesses int
	escapedUserProcesses int
}

func (snapshot ActivitySnapshot) RuntimeID() string { return snapshot.runtimeID }

func (snapshot ActivitySnapshot) ObservedAt() time.Time { return snapshot.observedAt }

func (snapshot ActivitySnapshot) GuestSequence() int64 { return snapshot.guestSequence }

func (snapshot ActivitySnapshot) SSHConnections() int { return snapshot.sshConnections }

func (snapshot ActivitySnapshot) CodexProcesses() int { return snapshot.codexProcesses }

func (snapshot ActivitySnapshot) ClaudeProcesses() int { return snapshot.claudeProcesses }

func (snapshot ActivitySnapshot) ProtectedProcesses() int { return snapshot.protectedProcesses }

func (snapshot ActivitySnapshot) SelectedContainers() int { return snapshot.selectedContainers }

func (snapshot ActivitySnapshot) UnknownUserProcesses() int { return snapshot.unknownUserProcesses }

// UserSessionProcesses is the number of non-baseline user-owned processes
// inside the cgroup-v2 subtree /user.slice/user-<uid>.slice.
func (snapshot ActivitySnapshot) UserSessionProcesses() int { return snapshot.userSessionProcesses }

// EscapedUserProcesses is the number of non-baseline user-owned processes
// outside the user's cgroup-v2 subtree. These processes are also unknown activity.
func (snapshot ActivitySnapshot) EscapedUserProcesses() int { return snapshot.escapedUserProcesses }

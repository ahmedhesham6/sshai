package guest_test

import (
	"context"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
)

func TestObserverSummarizesExplicitlyAllowedActivity(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, time.July, 13, 12, 0, 0, 123, time.FixedZone("UTC+2", 2*60*60))
	source := staticSource{sample: guest.ExternalSample{
		ObservedAt:    observedAt,
		GuestSequence: 7,
		ActiveSSHConnections: []guest.ActiveSSHConnectionSample{
			{ID: "ssh-2"},
			{ID: "ssh-1"},
		},
		Processes: []guest.ProcessSample{
			{PID: 30, ParentPID: 1, Executable: "/opt/agents/claude", State: guest.ProcessWaiting},
			{PID: 20, ParentPID: 1, Executable: "/usr/local/bin/codex", State: guest.ProcessRunning},
			{PID: 21, ParentPID: 20, Executable: "/usr/local/bin/codex", State: guest.ProcessWaiting},
			{PID: 40, ParentPID: 1, Executable: "/usr/bin/postgres", State: guest.ProcessWaiting},
			{PID: 50, ParentPID: 1, Executable: "/usr/bin/bash", State: guest.ProcessRunning},
		},
		Containers: []guest.ContainerSample{
			{ID: "container-db", Selection: "project-db", State: guest.ContainerRunning},
			{ID: "container-cache", Selection: "project-cache", State: guest.ContainerStopped},
			{ID: "container-other", Selection: "unselected", State: guest.ContainerRunning},
		},
	}}
	observer, err := guest.NewObserver("runtime-1", source, guest.Allowlists{
		CodexExecutables:     []string{"/usr/local/bin/codex"},
		ClaudeExecutables:    []string{"/opt/agents/claude"},
		ProtectedExecutables: []string{"/usr/bin/postgres"},
		SelectedContainers:   []string{"project-db", "project-cache"},
	})
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}

	snapshot, err := observer.Observe(context.Background())
	if err != nil {
		t.Fatalf("observe activity: %v", err)
	}

	if got := snapshot.RuntimeID(); got != "runtime-1" {
		t.Fatalf("runtime ID = %q, want runtime-1", got)
	}
	if got := snapshot.ObservedAt(); !got.Equal(observedAt) || got.Location() != time.UTC {
		t.Fatalf("observed at = %v, want the sampled instant normalized to UTC", got)
	}
	if got := snapshot.GuestSequence(); got != 7 {
		t.Fatalf("guest sequence = %d, want 7", got)
	}
	if got := snapshot.SSHConnections(); got != 2 {
		t.Fatalf("SSH connections = %d, want 2", got)
	}
	if got := snapshot.CodexProcesses(); got != 1 {
		t.Fatalf("Codex processes = %d, want one recognized process tree", got)
	}
	if got := snapshot.ClaudeProcesses(); got != 1 {
		t.Fatalf("Claude processes = %d, want waiting Claude process to count", got)
	}
	if got := snapshot.ProtectedProcesses(); got != 1 {
		t.Fatalf("protected processes = %d, want 1", got)
	}
	if got := snapshot.SelectedContainers(); got != 1 {
		t.Fatalf("selected running containers = %d, want 1", got)
	}
}

func TestObserverCountsUniqueSamplesDeterministically(t *testing.T) {
	t.Parallel()

	allowlists := guest.Allowlists{
		CodexExecutables:   []string{"codex"},
		SelectedContainers: []string{"db"},
	}
	observedAt := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	source := staticSource{sample: guest.ExternalSample{
		ObservedAt:           observedAt,
		GuestSequence:        1,
		ActiveSSHConnections: []guest.ActiveSSHConnectionSample{{ID: "ssh-1"}, {ID: "ssh-1"}},
		Processes: []guest.ProcessSample{
			{PID: 20, ParentPID: 10, Executable: "codex", State: guest.ProcessWaiting},
			{PID: 10, ParentPID: 1, Executable: "codex", State: guest.ProcessRunning},
			{PID: 20, ParentPID: 10, Executable: "codex", State: guest.ProcessWaiting},
		},
		Containers: []guest.ContainerSample{
			{ID: "db-1", Selection: "db", State: guest.ContainerRunning},
			{ID: "db-1", Selection: "db", State: guest.ContainerRunning},
		},
	}}
	observer, err := guest.NewObserver("runtime-1", source, allowlists)
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}
	allowlists.CodexExecutables[0] = "changed-after-construction"
	allowlists.SelectedContainers[0] = "changed-after-construction"

	snapshot, err := observer.Observe(context.Background())
	if err != nil {
		t.Fatalf("observe activity: %v", err)
	}
	if got := snapshot.SSHConnections(); got != 1 {
		t.Fatalf("SSH connections = %d, want one unique connection", got)
	}
	if got := snapshot.CodexProcesses(); got != 1 {
		t.Fatalf("Codex processes = %d, want one unique process tree", got)
	}
	if got := snapshot.SelectedContainers(); got != 1 {
		t.Fatalf("selected containers = %d, want one unique container", got)
	}
}

func TestObserverRejectsConflictingDuplicateProcessSamples(t *testing.T) {
	t.Parallel()

	source := staticSource{sample: guest.ExternalSample{
		ObservedAt:    time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC),
		GuestSequence: 1,
		Processes: []guest.ProcessSample{
			{PID: 10, ParentPID: 1, Executable: "codex", State: guest.ProcessRunning},
			{PID: 10, ParentPID: 1, Executable: "claude", State: guest.ProcessWaiting},
		},
	}}
	observer, err := guest.NewObserver("runtime-1", source, guest.Allowlists{
		CodexExecutables:  []string{"codex"},
		ClaudeExecutables: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}

	if _, err = observer.Observe(context.Background()); err == nil {
		t.Fatal("conflicting samples for one process identity were accepted")
	}
}

func TestObserverRejectsAmbiguousProcessAllowlists(t *testing.T) {
	t.Parallel()

	source := staticSource{}
	for name, allowlists := range map[string]guest.Allowlists{
		"empty executable": {
			CodexExecutables: []string{""},
		},
		"overlapping categories": {
			CodexExecutables:     []string{"codex"},
			ProtectedExecutables: []string{"codex"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := guest.NewObserver("runtime-1", source, allowlists); err == nil {
				t.Fatal("ambiguous allowlist was accepted")
			}
		})
	}
}

func TestObserverRejectsMalformedExternalSamples(t *testing.T) {
	t.Parallel()

	valid := guest.ExternalSample{
		ObservedAt:    time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC),
		GuestSequence: 1,
	}
	for name, mutate := range map[string]func(*guest.ExternalSample){
		"missing SSH identity": func(sample *guest.ExternalSample) {
			sample.ActiveSSHConnections = []guest.ActiveSSHConnectionSample{{}}
		},
		"invalid process identity": func(sample *guest.ExternalSample) {
			sample.Processes = []guest.ProcessSample{{PID: 0, ParentPID: 1, Executable: "codex", State: guest.ProcessRunning}}
		},
		"invalid process state": func(sample *guest.ExternalSample) {
			sample.Processes = []guest.ProcessSample{{PID: 10, ParentPID: 1, Executable: "codex", State: "exited"}}
		},
		"cyclic process ancestry": func(sample *guest.ExternalSample) {
			sample.Processes = []guest.ProcessSample{
				{PID: 10, ParentPID: 20, Executable: "codex", State: guest.ProcessRunning},
				{PID: 20, ParentPID: 10, Executable: "codex", State: guest.ProcessWaiting},
			}
		},
		"conflicting container identity": func(sample *guest.ExternalSample) {
			sample.Containers = []guest.ContainerSample{
				{ID: "db-1", Selection: "db", State: guest.ContainerRunning},
				{ID: "db-1", Selection: "db", State: guest.ContainerStopped},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sample := valid
			mutate(&sample)
			observer, err := guest.NewObserver("runtime-1", staticSource{sample: sample}, guest.Allowlists{})
			if err != nil {
				t.Fatalf("create observer: %v", err)
			}
			if _, err = observer.Observe(context.Background()); err == nil {
				t.Fatal("malformed external sample was accepted")
			}
		})
	}
}

type staticSource struct {
	sample guest.ExternalSample
	err    error
}

func (source staticSource) Sample(context.Context) (guest.ExternalSample, error) {
	return source.sample, source.err
}

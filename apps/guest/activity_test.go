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
		UserUID:       1000,
		ActiveSSHConnections: []guest.ActiveSSHConnectionSample{
			{ID: "ssh-2"},
			{ID: "ssh-1"},
		},
		Processes: []guest.ProcessSample{
			userProcess(30, 1, "/opt/agents/claude", guest.ProcessWaiting),
			userProcess(20, 1, "/usr/local/bin/codex", guest.ProcessRunning),
			userProcess(21, 20, "/usr/local/bin/codex", guest.ProcessWaiting),
			{PID: 40, ParentPID: 1, OwnerUID: 999, Executable: "/usr/bin/postgres", State: guest.ProcessWaiting, CgroupPath: "/system.slice/postgresql.service"},
			userProcess(50, 1, "/usr/bin/bash", guest.ProcessRunning),
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
	if got := snapshot.UnknownUserProcesses(); got != 1 {
		t.Fatalf("unknown user processes = %d, want 1", got)
	}
	if got := snapshot.UserSessionProcesses(); got != 4 {
		t.Fatalf("user-session processes = %d, want 4", got)
	}
	if got := snapshot.EscapedUserProcesses(); got != 0 {
		t.Fatalf("escaped user processes = %d, want 0", got)
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
		UserUID:              1000,
		ActiveSSHConnections: []guest.ActiveSSHConnectionSample{{ID: "ssh-1"}, {ID: "ssh-1"}},
		Processes: []guest.ProcessSample{
			userProcess(20, 10, "codex", guest.ProcessWaiting),
			userProcess(10, 1, "codex", guest.ProcessRunning),
			userProcess(20, 10, "codex", guest.ProcessWaiting),
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
		UserUID:       1000,
		Processes: []guest.ProcessSample{
			userProcess(10, 1, "codex", guest.ProcessRunning),
			userProcess(10, 1, "claude", guest.ProcessWaiting),
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
		UserUID:       1000,
	}
	for name, mutate := range map[string]func(*guest.ExternalSample){
		"missing user UID": func(sample *guest.ExternalSample) {
			sample.UserUID = 0
		},
		"missing SSH identity": func(sample *guest.ExternalSample) {
			sample.ActiveSSHConnections = []guest.ActiveSSHConnectionSample{{}}
		},
		"invalid process identity": func(sample *guest.ExternalSample) {
			process := userProcess(10, 1, "codex", guest.ProcessRunning)
			process.PID = 0
			sample.Processes = []guest.ProcessSample{process}
		},
		"invalid process state": func(sample *guest.ExternalSample) {
			sample.Processes = []guest.ProcessSample{userProcess(10, 1, "codex", "exited")}
		},
		"invalid process owner": func(sample *guest.ExternalSample) {
			process := userProcess(10, 1, "codex", guest.ProcessRunning)
			process.OwnerUID = -1
			sample.Processes = []guest.ProcessSample{process}
		},
		"invalid cgroup path": func(sample *guest.ExternalSample) {
			process := userProcess(10, 1, "codex", guest.ProcessRunning)
			process.CgroupPath = "user.slice/user-1000.slice/session-1.scope"
			sample.Processes = []guest.ProcessSample{process}
		},
		"cyclic process ancestry": func(sample *guest.ExternalSample) {
			sample.Processes = []guest.ProcessSample{
				userProcess(10, 20, "codex", guest.ProcessRunning),
				userProcess(20, 10, "codex", guest.ProcessWaiting),
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

func TestObserverClassifiesUnknownAndCgroupScopedProcesses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		processes   []guest.ProcessSample
		containers  []guest.ContainerSample
		unknown     int
		codex       int
		protected   int
		userSession int
		escapedUser int
	}{
		{
			name:        "unrecognized user process is unknown",
			processes:   []guest.ProcessSample{userProcess(10, 1, "/usr/bin/make", guest.ProcessRunning)},
			unknown:     1,
			userSession: 1,
		},
		{
			name: "root-owned process in user subtree is unknown",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 0, Executable: "/usr/bin/make", State: guest.ProcessRunning, CgroupPath: "/user.slice/user-1000.slice/session-1.scope"},
			},
			unknown:     1,
			userSession: 1,
		},
		{
			name: "setuid process in user subtree is unknown",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 0, Executable: "/usr/bin/passwd", State: guest.ProcessWaiting, CgroupPath: "/user.slice/user-1000.slice/session-1.scope"},
			},
			unknown:     1,
			userSession: 1,
		},
		{
			name: "other uid is explicit system baseline",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 999, Executable: "/usr/lib/systemd/systemd-journald", State: guest.ProcessWaiting, CgroupPath: "/system.slice/systemd-journald.service"},
			},
		},
		{
			name: "root system daemon outside user subtree is baseline",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 0, Executable: "/usr/lib/systemd/systemd-journald", State: guest.ProcessWaiting, CgroupPath: "/system.slice/systemd-journald.service"},
			},
		},
		{
			name: "systemd user manager exact init scope is baseline",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 1000, Executable: "/usr/lib/systemd/systemd", State: guest.ProcessWaiting, CgroupPath: "/user.slice/user-1000.slice/user@1000.service/init.scope"},
			},
		},
		{
			name: "systemd executable outside exact init scope is unknown",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 1000, Executable: "/usr/lib/systemd/systemd", State: guest.ProcessWaiting, CgroupPath: "/user.slice/user-1000.slice/session-1.scope"},
			},
			unknown:     1,
			userSession: 1,
		},
		{
			name: "agent descendants belong to the recognized tree",
			processes: []guest.ProcessSample{
				userProcess(10, 1, "/usr/local/bin/codex", guest.ProcessRunning),
				userProcess(11, 10, "/usr/bin/bash", guest.ProcessWaiting),
			},
			codex:       1,
			userSession: 2,
		},
		{
			name: "escaped recognized executable becomes unknown",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 1000, Executable: "/usr/local/bin/codex", State: guest.ProcessRunning, CgroupPath: "/system.slice/escaped.scope"},
			},
			unknown:     1,
			escapedUser: 1,
		},
		{
			name: "agent descendant outside tracked cgroup becomes unknown",
			processes: []guest.ProcessSample{
				userProcess(10, 1, "/usr/local/bin/codex", guest.ProcessRunning),
				{PID: 11, ParentPID: 10, OwnerUID: 1000, Executable: "/usr/bin/bash", State: guest.ProcessWaiting, CgroupPath: "/user.slice/user-1000.slice/session-2.scope"},
			},
			unknown:     1,
			codex:       1,
			userSession: 2,
		},
		{
			name: "wrong user slice is escaped",
			processes: []guest.ProcessSample{
				{PID: 10, ParentPID: 1, OwnerUID: 1000, Executable: "/usr/bin/make", State: guest.ProcessRunning, CgroupPath: "/user.slice/user-2000.slice/session-1.scope"},
			},
			unknown:     1,
			escapedUser: 1,
		},
		{
			name: "protected user process is classified",
			processes: []guest.ProcessSample{
				userProcess(10, 1, "/usr/bin/postgres", guest.ProcessWaiting),
			},
			protected:   1,
			userSession: 1,
		},
		{
			name: "selected rootless container process is classified",
			processes: []guest.ProcessSample{
				userContainerProcess(10, "container-db"),
			},
			containers: []guest.ContainerSample{
				{ID: "container-db", Selection: "project-db", State: guest.ContainerRunning},
			},
			userSession: 1,
		},
		{
			name: "unselected rootless container process is unknown",
			processes: []guest.ProcessSample{
				userContainerProcess(10, "container-other"),
			},
			containers: []guest.ContainerSample{
				{ID: "container-other", Selection: "other", State: guest.ContainerRunning},
			},
			unknown:     1,
			userSession: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observer, err := guest.NewObserver("runtime-1", staticSource{sample: guest.ExternalSample{
				ObservedAt:    time.Date(2026, time.July, 18, 10, 0, 0, 0, time.UTC),
				GuestSequence: 1,
				UserUID:       1000,
				Processes:     test.processes,
				Containers:    test.containers,
			}}, guest.Allowlists{
				CodexExecutables:     []string{"/usr/local/bin/codex"},
				ProtectedExecutables: []string{"/usr/bin/postgres"},
				SelectedContainers:   []string{"project-db"},
			})
			if err != nil {
				t.Fatalf("create observer: %v", err)
			}

			snapshot, err := observer.Observe(context.Background())
			if err != nil {
				t.Fatalf("observe activity: %v", err)
			}
			if got := snapshot.UnknownUserProcesses(); got != test.unknown {
				t.Errorf("unknown user processes = %d, want %d", got, test.unknown)
			}
			if got := snapshot.CodexProcesses(); got != test.codex {
				t.Errorf("Codex processes = %d, want %d", got, test.codex)
			}
			if got := snapshot.ProtectedProcesses(); got != test.protected {
				t.Errorf("protected processes = %d, want %d", got, test.protected)
			}
			if got := snapshot.UserSessionProcesses(); got != test.userSession {
				t.Errorf("user-session processes = %d, want %d", got, test.userSession)
			}
			if got := snapshot.EscapedUserProcesses(); got != test.escapedUser {
				t.Errorf("escaped user processes = %d, want %d", got, test.escapedUser)
			}
		})
	}
}

func userProcess(pid, parentPID int, executable string, state guest.ProcessState) guest.ProcessSample {
	return guest.ProcessSample{
		PID:        pid,
		ParentPID:  parentPID,
		OwnerUID:   1000,
		Executable: executable,
		State:      state,
		CgroupPath: "/user.slice/user-1000.slice/session-1.scope",
	}
}

func userContainerProcess(pid int, containerID string) guest.ProcessSample {
	process := userProcess(pid, 1, "/usr/bin/container-process", guest.ProcessRunning)
	process.ContainerID = containerID
	return process
}

type staticSource struct {
	sample guest.ExternalSample
	err    error
}

func (source staticSource) Sample(context.Context) (guest.ExternalSample, error) {
	return source.sample, source.err
}

package application_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
)

func TestCreateEnvironmentReservesAndSubmitsIdempotently(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repository := &creationRepositoryFake{}
	workflow := &workflowFake{}
	service := application.NewCreateEnvironmentService(
		repository,
		workflow,
		&idsFake{values: []string{"environment-1", "policy-1", "operation-1", "unused-1", "unused-2", "unused-3"}},
		func() time.Time { return now },
		map[string]string{"us-east-1": "us-east-1a"},
	)
	input := application.CreateEnvironmentInput{
		OwnerUserID:      "user-1",
		Name:             "API Workspace",
		Region:           "us-east-1",
		RuntimePreset:    "standard",
		ProfileVersionID: "profile-version-1",
		ProjectSeedID:    "project-seed-1",
		SSHKeyIDs:        []string{"ssh-key-1"},
		AutoStopMode:     "when_agents_finish",
		GracePeriod:      300,
		IdempotencyKey:   "request-key-0001",
	}

	created, err := service.CreateEnvironment(t.Context(), input)
	if err != nil {
		t.Fatalf("CreateEnvironment(): %v", err)
	}
	snapshot := created.Environment().Snapshot()
	if snapshot.ID != "environment-1" || snapshot.Slug != "api-workspace" || snapshot.AvailabilityZone != "us-east-1a" {
		t.Fatalf("Environment = %#v", snapshot)
	}
	if got := created.Policy().Snapshot(); got.ID != "policy-1" || got.EnvironmentID != snapshot.ID || got.GracePeriodSeconds != 300 {
		t.Fatalf("Auto-stop Policy = %#v", got)
	}
	if got := created.Operation().Snapshot(); got.ID != "operation-1" || got.EnvironmentID != snapshot.ID || got.RestateInvocationID != nil {
		t.Fatalf("Operation = %#v", got)
	}
	if got, want := repository.saved.ProjectSeedID(), "project-seed-1"; got != want {
		t.Fatalf("Project Seed ID = %q, want %q", got, want)
	}
	if got, want := repository.saved.SSHKeyIDs(), []string{"ssh-key-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SSH Key IDs = %#v, want %#v", got, want)
	}
	if got, want := workflow.operationIDs(), []string{"operation-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workflow submissions = %#v, want %#v", got, want)
	}

	replayed, err := service.CreateEnvironment(t.Context(), input)
	if err != nil {
		t.Fatalf("replay CreateEnvironment(): %v", err)
	}
	if replayed.Environment().Snapshot().ID != snapshot.ID || replayed.Operation().Snapshot().ID != "operation-1" {
		t.Fatalf("replay returned a different reservation: %#v", replayed)
	}
	if got, want := workflow.operationIDs(), []string{"operation-1", "operation-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workflow replay submissions = %#v, want %#v", got, want)
	}
}

func TestCreateEnvironmentCanRetryWorkflowSubmissionAfterReservation(t *testing.T) {
	repository := &creationRepositoryFake{}
	workflow := &workflowFake{err: errors.New("Restate unavailable")}
	service := application.NewCreateEnvironmentService(
		repository,
		workflow,
		&idsFake{values: []string{"environment-1", "policy-1", "operation-1", "unused-1", "unused-2", "unused-3"}},
		func() time.Time { return time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC) },
		map[string]string{"us-east-1": "us-east-1a"},
	)
	input := validCreateEnvironmentInput()
	if _, err := service.CreateEnvironment(t.Context(), input); err == nil {
		t.Fatal("CreateEnvironment() error = nil")
	}
	workflow.err = nil
	if _, err := service.CreateEnvironment(t.Context(), input); err != nil {
		t.Fatalf("retry CreateEnvironment(): %v", err)
	}
	if repository.reserveCalls != 2 || len(workflow.ids) != 2 {
		t.Fatalf("reserve calls = %d, workflow calls = %d", repository.reserveCalls, len(workflow.ids))
	}
}

func TestCreateEnvironmentRejectsUnavailableRegion(t *testing.T) {
	repository := &creationRepositoryFake{}
	workflow := &workflowFake{}
	service := application.NewCreateEnvironmentService(
		repository,
		workflow,
		&idsFake{},
		time.Now,
		map[string]string{"us-east-1": "us-east-1a"},
	)
	input := validCreateEnvironmentInput()
	input.Region = "moon-1"
	if _, err := service.CreateEnvironment(t.Context(), input); !errors.Is(err, application.ErrRegionUnavailable) {
		t.Fatalf("CreateEnvironment() error = %v, want ErrRegionUnavailable", err)
	}
	if repository.reserveCalls != 0 || len(workflow.ids) != 0 {
		t.Fatal("invalid command reached repository or workflow")
	}
}

func validCreateEnvironmentInput() application.CreateEnvironmentInput {
	return application.CreateEnvironmentInput{
		OwnerUserID:      "user-1",
		Name:             "API Workspace",
		Region:           "us-east-1",
		RuntimePreset:    "standard",
		ProfileVersionID: "profile-version-1",
		ProjectSeedID:    "project-seed-1",
		SSHKeyIDs:        []string{"ssh-key-1"},
		AutoStopMode:     "manual",
		GracePeriod:      0,
		IdempotencyKey:   "request-key-0001",
	}
}

type creationRepositoryFake struct {
	saved        application.EnvironmentCreation
	reserveCalls int
}

func (repository *creationRepositoryFake) ReserveEnvironmentCreation(_ context.Context, candidate application.EnvironmentCreation) (application.EnvironmentCreation, error) {
	repository.reserveCalls++
	if repository.saved.Environment().Snapshot().ID != "" {
		return repository.saved, nil
	}
	repository.saved = candidate
	return candidate, nil
}

type workflowFake struct {
	ids []string
	err error
}

func (workflow *workflowFake) DispatchEnvironmentCreate(_ context.Context, operationID string) error {
	workflow.ids = append(workflow.ids, operationID)
	return workflow.err
}

func (workflow *workflowFake) operationIDs() []string {
	return append([]string(nil), workflow.ids...)
}

type idsFake struct {
	values []string
	index  int
}

func (ids *idsFake) NewID() string {
	value := ids.values[ids.index]
	ids.index++
	return value
}

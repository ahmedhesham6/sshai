package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

var ErrRegionUnavailable = errors.New("region unavailable")

type CreateEnvironmentInput struct {
	OwnerUserID      string
	Name             string
	Region           string
	RuntimePreset    string
	ProfileVersionID string
	ProjectSeedID    string
	SSHKeyIDs        []string
	AutoStopMode     domain.AutoStopMode
	GracePeriod      int
	IdempotencyKey   string
}

type EnvironmentCreation = domain.EnvironmentCreation

type EnvironmentCreationRepository interface {
	ReserveEnvironmentCreation(context.Context, EnvironmentCreation) (EnvironmentCreation, error)
}

type EnvironmentCreateDispatcher interface {
	DispatchEnvironmentCreate(context.Context, string) error
}

type EnvironmentCreateWorkflowInput = domain.EnvironmentCreateDispatch

type IDGenerator interface {
	NewID() string
}

type CreateEnvironmentService struct {
	repository        EnvironmentCreationRepository
	dispatcher        EnvironmentCreateDispatcher
	ids               IDGenerator
	now               func() time.Time
	availabilityZones map[string]string
}

func NewCreateEnvironmentService(
	repository EnvironmentCreationRepository,
	dispatcher EnvironmentCreateDispatcher,
	ids IDGenerator,
	now func() time.Time,
	availabilityZones map[string]string,
) *CreateEnvironmentService {
	zones := make(map[string]string, len(availabilityZones))
	for region, zone := range availabilityZones {
		zones[region] = zone
	}
	return &CreateEnvironmentService{
		repository:        repository,
		dispatcher:        dispatcher,
		ids:               ids,
		now:               now,
		availabilityZones: zones,
	}
}

func (service *CreateEnvironmentService) CreateEnvironment(ctx context.Context, input CreateEnvironmentInput) (EnvironmentCreation, error) {
	availabilityZone, available := service.availabilityZones[input.Region]
	if !available {
		return EnvironmentCreation{}, fmt.Errorf("create Environment: %w: %s", ErrRegionUnavailable, input.Region)
	}
	canonicalInput, err := json.Marshal(input)
	if err != nil {
		return EnvironmentCreation{}, fmt.Errorf("create Environment: encode command: %w", err)
	}

	environmentID := service.ids.NewID()
	policyID := service.ids.NewID()
	operationID := service.ids.NewID()
	createdAt := service.now()
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID:                     environmentID,
		OwnerUserID:            input.OwnerUserID,
		Name:                   input.Name,
		Slug:                   slugify(input.Name),
		Region:                 input.Region,
		AvailabilityZone:       availabilityZone,
		RuntimePreset:          input.RuntimePreset,
		PinnedProfileVersionID: input.ProfileVersionID,
		AutoStopPolicyID:       policyID,
		CreatedAt:              createdAt,
	})
	if err != nil {
		return EnvironmentCreation{}, err
	}
	policy, err := domain.NewAutoStopPolicy(policyID, environmentID, input.AutoStopMode, input.GracePeriod)
	if err != nil {
		return EnvironmentCreation{}, err
	}
	operation, err := domain.QueueOperation(domain.OperationRequest{
		ID:                operationID,
		EnvironmentID:     environmentID,
		Type:              domain.OperationEnvironmentCreate,
		RequestedByUserID: input.OwnerUserID,
		IdempotencyKey:    input.IdempotencyKey,
		Input:             canonicalInput,
		CreatedAt:         createdAt,
	})
	if err != nil {
		return EnvironmentCreation{}, err
	}

	candidate, err := domain.NewEnvironmentCreation(environment, policy, operation, input.ProjectSeedID, input.SSHKeyIDs)
	if err != nil {
		return EnvironmentCreation{}, err
	}
	creation, err := service.repository.ReserveEnvironmentCreation(ctx, candidate)
	if err != nil {
		return EnvironmentCreation{}, fmt.Errorf("create Environment: reserve projection: %w", err)
	}
	if err := service.dispatcher.DispatchEnvironmentCreate(ctx, creation.Operation().Snapshot().ID); err != nil {
		return EnvironmentCreation{}, fmt.Errorf("create Environment: submit workflow: %w", err)
	}
	return creation, nil
}

func slugify(name string) string {
	var slug strings.Builder
	separator := false
	for _, character := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			if separator && slug.Len() > 0 {
				slug.WriteByte('-')
			}
			slug.WriteRune(character)
			separator = false
		} else {
			separator = true
		}
	}
	return slug.String()
}

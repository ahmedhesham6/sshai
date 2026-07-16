package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Component is the domain representation of one independently updateable
// item in a Capsule. Keep enum values aligned with libs/capsule/model.go; the
// domain package intentionally does not import the packaging-side model.
type Component struct {
	ID           string
	Type         ComponentType
	MediaType    string
	Digest       string
	SizeBytes    int64
	Scope        ComponentScope
	TrustClass   TrustClass
	Requirements ComponentRequirements
	// Content is optional resolver-side content for mergeable JSON/TOML
	// configuration. Capsule layers remain the durable source of truth.
	Content []byte
	// Provenance records the source Capsule Ref for each effective config key.
	Provenance map[string]string
}

type ComponentRequirements struct {
	Commands []string
	Secrets  []string
}

type ComponentType string

const (
	ComponentConfig           ComponentType = "config"
	ComponentSkill            ComponentType = "skill"
	ComponentCommand          ComponentType = "command"
	ComponentSubagent         ComponentType = "subagent"
	ComponentHook             ComponentType = "hook"
	ComponentIntegration      ComponentType = "integration"
	ComponentPermissionPolicy ComponentType = "permission-policy"
	ComponentTemplate         ComponentType = "template"
	ComponentExtension        ComponentType = "extension"

	ComponentTypeConfig           = ComponentConfig
	ComponentTypeSkill            = ComponentSkill
	ComponentTypeCommand          = ComponentCommand
	ComponentTypeSubagent         = ComponentSubagent
	ComponentTypeHook             = ComponentHook
	ComponentTypeIntegration      = ComponentIntegration
	ComponentTypePermissionPolicy = ComponentPermissionPolicy
	ComponentTypeTemplate         = ComponentTemplate
	ComponentTypeExtension        = ComponentExtension
)

type ComponentScope string

const (
	ScopeUser    ComponentScope = "user"
	ScopeProject ComponentScope = "project"

	ComponentScopeUser    = ScopeUser
	ComponentScopeProject = ScopeProject
)

type TrustClass string

const (
	TrustDeclarative TrustClass = "declarative"
	TrustExecutable  TrustClass = "executable"
	TrustPermission  TrustClass = "permission"

	ComponentTrustDeclarative = TrustDeclarative
	ComponentTrustExecutable  = TrustExecutable
	ComponentTrustPermission  = TrustPermission
)

func (component Component) Validate() error {
	if strings.TrimSpace(component.ID) == "" {
		return errors.New("Component ID is required")
	}
	if !component.Type.Valid() {
		return fmt.Errorf("Component type %q is invalid", component.Type)
	}
	prefix, suffix, found := strings.Cut(component.ID, ":")
	if !found || prefix != string(component.Type) || strings.TrimSpace(suffix) == "" || strings.ContainsAny(component.ID, " \t\r\n") {
		return errors.New("Component ID must be type-qualified")
	}
	if strings.TrimSpace(component.MediaType) == "" {
		return errors.New("Component media type is required")
	}
	if !contentDigestPattern.MatchString(component.Digest) {
		return errors.New("Component digest must be a SHA-256 digest")
	}
	if component.SizeBytes < 0 {
		return errors.New("Component size cannot be negative")
	}
	if !component.Scope.Valid() {
		return fmt.Errorf("Component scope %q is invalid", component.Scope)
	}
	if !component.TrustClass.Valid() {
		return fmt.Errorf("Component trust class %q is invalid", component.TrustClass)
	}
	if required, constrained := requiredTrustClass(component.Type); constrained && component.TrustClass != required {
		return fmt.Errorf("Component type %q requires trust class %q, got %q", component.Type, required, component.TrustClass)
	}
	for _, requirement := range component.Requirements.Commands {
		if strings.TrimSpace(requirement) == "" {
			return errors.New("Component command requirements must be non-empty")
		}
	}
	for _, requirement := range component.Requirements.Secrets {
		if strings.TrimSpace(requirement) == "" {
			return errors.New("Component secret requirements must be non-empty")
		}
	}
	return nil
}

func requiredTrustClass(componentType ComponentType) (TrustClass, bool) {
	switch componentType {
	case ComponentHook, ComponentExtension:
		return TrustExecutable, true
	case ComponentPermissionPolicy:
		return TrustPermission, true
	default:
		return "", false
	}
}

func ValidateComponent(component Component) error {
	return component.Validate()
}

func NewComponent(component Component) (Component, error) {
	if err := component.Validate(); err != nil {
		return Component{}, err
	}
	return component, nil
}

func (componentType ComponentType) Valid() bool {
	switch componentType {
	case ComponentConfig, ComponentSkill, ComponentCommand, ComponentSubagent, ComponentHook,
		ComponentIntegration, ComponentPermissionPolicy, ComponentTemplate, ComponentExtension:
		return true
	default:
		return false
	}
}

func (scope ComponentScope) Valid() bool {
	return scope == ScopeUser || scope == ScopeProject
}

func (trust TrustClass) Valid() bool {
	return trust == TrustDeclarative || trust == TrustExecutable || trust == TrustPermission
}

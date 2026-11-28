package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kirin7002/app_personal_agent/contract"
)

//go:embed specs/demo.json
var demoCatalogJSON []byte

//go:embed specs/business_mcp.json
var businessMCPCatalogJSON []byte

// CatalogBundle is the versioned declarative capability definition loaded from JSON.
type CatalogBundle struct {
	Version             string            `json:"version"`
	DefaultDisableTools []string          `json:"default_disable_tools,omitempty"`
	Operations          []OperationConfig `json:"operations"`
}

// OperationConfig is the JSON-serializable declarative representation of an OperationSpec.
type OperationConfig struct {
	Device            string              `json:"device"`
	Tool              string              `json:"tool"`
	MCPTool           string              `json:"mcp_tool"`
	DownstreamTool    string              `json:"downstream_tool,omitempty"`
	Title             string              `json:"title,omitempty"`
	Category          string              `json:"category,omitempty"`
	Description       string              `json:"description"`
	Aliases           []string            `json:"aliases,omitempty"`
	Utterances        []string            `json:"utterances,omitempty"`
	AntiExamples      []string            `json:"anti_examples,omitempty"`
	GateKeywords      []string            `json:"gate_keywords,omitempty"`
	GatedTerms        []string            `json:"gated_terms,omitempty"`
	DisabledByDefault bool                `json:"disabled_by_default,omitempty"`
	MutuallyExclusive [][]string          `json:"mutually_exclusive,omitempty"`
	RequireAnyOf      [][]string          `json:"require_any_of,omitempty"`
	MissingQuestions  map[string]string   `json:"missing_questions,omitempty"`
	StatusSignals     []string            `json:"status_signals,omitempty"`
	Fields            []FieldSpec         `json:"fields"`
	Guard             *GuardSpec          `json:"guard,omitempty"`
}

// GuardSpec holds catalog-driven intent guard metadata used by vehicle/mcp/guard.go.
type GuardSpec struct {
	DeviceKeywords         []string `json:"device_keywords,omitempty"`
	PositiveVerbs          []string `json:"positive_verbs,omitempty"`
	ConflictVerbs          []string `json:"conflict_verbs,omitempty"`
	RelativeActionKeywords []string `json:"relative_action_keywords,omitempty"`
	SensitiveConfirm       bool     `json:"sensitive_confirm,omitempty"`
}

// FieldExtractorSpec defines declarative slot value extraction from user utterances.
type FieldExtractorSpec struct {
	Kind            string              `json:"kind"` // "enum_alias", "number", "integer", "relative_tune", "free_text"
	ValueAliases    map[string][]string `json:"value_aliases,omitempty"`
	UnitPattern     string              `json:"unit_pattern,omitempty"`
	Min             *float64            `json:"min,omitempty"`
	Max             *float64            `json:"max,omitempty"`
	AllowBareNumber bool                `json:"allow_bare_number,omitempty"`
}

var validFieldTypes = map[string]bool{
	"string":        true,
	"number":        true,
	"integer":       true,
	"boolean":       true,
	"array_string":  true,
	"array_integer": true,
	"object":        true,
	"array_object":  true,
}

// ParseCatalogJSON parses and validates a versioned declarative capability bundle.
func ParseCatalogJSON(raw []byte) (CatalogBundle, []OperationSpec, error) {
	var bundle CatalogBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return CatalogBundle{}, nil, fmt.Errorf("parse catalog json: %w", err)
	}
	if strings.TrimSpace(bundle.Version) == "" {
		return CatalogBundle{}, nil, fmt.Errorf("catalog version is required")
	}
	if len(bundle.Operations) == 0 {
		return CatalogBundle{}, nil, fmt.Errorf("catalog must define at least one operation")
	}

	disabledSet := make(map[string]bool, len(bundle.DefaultDisableTools))
	for _, t := range bundle.DefaultDisableTools {
		if trimmed := strings.TrimSpace(t); trimmed != "" {
			disabledSet[trimmed] = true
		}
	}

	seen := make(map[contract.Operation]bool, len(bundle.Operations))
	specs := make([]OperationSpec, 0, len(bundle.Operations))
	for i, cfg := range bundle.Operations {
		op := contract.Operation{
			Device: strings.TrimSpace(cfg.Device),
			Tool:   strings.TrimSpace(cfg.Tool),
		}
		if op.Device == "" || op.Tool == "" {
			return CatalogBundle{}, nil, fmt.Errorf("operation[%d]: device and tool are required", i)
		}
		if seen[op] {
			return CatalogBundle{}, nil, fmt.Errorf("operation %s.%s duplicated in catalog", op.Device, op.Tool)
		}
		seen[op] = true
		if strings.TrimSpace(cfg.MCPTool) == "" {
			return CatalogBundle{}, nil, fmt.Errorf("operation %s.%s: mcp_tool is required", op.Device, op.Tool)
		}
		if err := validateFieldSpecs(op, cfg.Fields); err != nil {
			return CatalogBundle{}, nil, err
		}
		disabled := cfg.DisabledByDefault || disabledSet[op.Tool] || (cfg.DownstreamTool != "" && disabledSet[cfg.DownstreamTool])
		title := strings.TrimSpace(cfg.Title)
		if title == "" {
			title = cfg.Description
		}
		specs = append(specs, OperationSpec{
			Operation:         op,
			MCPTool:           cfg.MCPTool,
			DownstreamTool:    cfg.DownstreamTool,
			Title:             title,
			Category:          cfg.Category,
			Description:       cfg.Description,
			Aliases:           cfg.Aliases,
			Utterances:        cfg.Utterances,
			AntiExamples:      cfg.AntiExamples,
			GateKeywords:      cfg.GateKeywords,
			GatedTerms:        cfg.GatedTerms,
			DisabledByDefault: disabled,
			MutuallyExclusive: cfg.MutuallyExclusive,
			RequireAnyOf:      cfg.RequireAnyOf,
			MissingQuestions:  cfg.MissingQuestions,
			StatusSignals:     cfg.StatusSignals,
			Fields:            cfg.Fields,
			Guard:             cfg.Guard,
		})
	}
	return bundle, specs, nil
}

func validateFieldSpecs(op contract.Operation, fields []FieldSpec) error {
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			return fmt.Errorf("operation %s.%s: field name is required", op.Device, op.Tool)
		}
		if seen[name] {
			return fmt.Errorf("operation %s.%s: duplicate field %q", op.Device, op.Tool, name)
		}
		seen[name] = true
		if !validFieldTypes[f.Type] {
			return fmt.Errorf("operation %s.%s: unsupported field %q type %q", op.Device, op.Tool, name, f.Type)
		}
		if len(f.Properties) > 0 {
			if err := validateFieldSpecs(op, f.Properties); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadCatalogJSON replaces the service's operation specs from a declarative JSON bundle.
func (s *Service) LoadCatalogJSON(raw []byte) error {
	bundle, specs, err := ParseCatalogJSON(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Version = bundle.Version
	s.specs = make(map[contract.Operation]OperationSpec, len(specs))
	s.order = make([]contract.Operation, 0, len(specs))
	for _, sp := range specs {
		s.order = append(s.order, sp.Operation)
		s.specs[sp.Operation] = sp
	}
	s.onCatalogChangedLocked()
	return nil
}

// LoadCatalogFile loads a declarative capability catalog from disk.
func (s *Service) LoadCatalogFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read catalog file %q: %w", path, err)
	}
	return s.LoadCatalogJSON(raw)
}

// NewBusinessMCPService constructs a catalog preloaded with all 31 business MCP
// server tool definitions (26 enabled by default, 5 disabled by default_disable_tools).
func NewBusinessMCPService(telemetry TelemetryProvider) (*Service, error) {
	if telemetry == nil {
		telemetry = NewMemoryTelemetry()
	}
	s := &Service{
		Version:   DefaultCatalogVersion,
		specs:     make(map[contract.Operation]OperationSpec),
		profiles:  make(map[string]VehicleProfile),
		telemetry: telemetry,
		manuals:   defaultManualEntries(),
	}
	if err := s.LoadCatalogJSON(businessMCPCatalogJSON); err != nil {
		return nil, err
	}
	return s, nil
}

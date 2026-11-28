// Package catalog implements the versioned declarative vehicle capability map,
// BM25 recall, single-source parameter schema validation, telemetry status reading
// (with command confirmation helpers), and versioned vehicle manual search (H06 / P0-1).
package catalog

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kirin7002/app_personal_agent/contract"
)

const DefaultCatalogVersion = "2026.1"

var WindowPositions = []string{"front_left", "front_right", "rear_left", "rear_right"}

// VehicleProfile describes model-specific capability constraints, tool toggles, and manual version.
type VehicleProfile struct {
	VehicleID     string
	Model         string
	ManualVersion string
	MinTempC      float64
	MaxTempC      float64
	// DisabledOps optionally hides specific canonical operations for a vehicle trim.
	DisabledOps map[contract.Operation]bool
	// DisabledTools hides operations matching tool or downstream_tool name (nacos disable_tools).
	DisabledTools map[string]bool
	// EnabledTools explicitly re-enables operations marked disabled_by_default.
	EnabledTools map[string]bool
}

func DefaultProfile(vehicleID string) VehicleProfile {
	return VehicleProfile{
		VehicleID:     vehicleID,
		Model:         "EV-SUV-2026",
		ManualVersion: DefaultCatalogVersion,
		MinTempC:      16,
		MaxTempC:      32,
	}
}

// FieldSpec is the single source of truth for both JSON Schema emission and Go validation.
type FieldSpec struct {
	Name         string              `json:"name"`
	Type         string              `json:"type"` // "string","number","integer","boolean","array_string","array_integer","object","array_object"
	Description  string              `json:"description,omitempty"`
	Required     bool                `json:"required,omitempty"`
	Enum         []string            `json:"enum,omitempty"`
	EnumLabels   map[string][]string `json:"enum_labels,omitempty"`
	Min          *float64            `json:"min,omitempty"`
	Max          *float64            `json:"max,omitempty"`
	MinItems     int                 `json:"min_items,omitempty"`
	Question     string              `json:"question,omitempty"`
	DynamicBound string              `json:"dynamic_bound,omitempty"` // e.g. "temperature_c"
	Sensitive    bool                `json:"sensitive,omitempty"`
	Properties   []FieldSpec         `json:"properties,omitempty"`
	Extractor    *FieldExtractorSpec `json:"extractor,omitempty"`
}

func (f FieldSpec) isSensitive() bool {
	return f.Sensitive || IsSensitiveFieldName(f.Name)
}

func applyFieldDynamicBounds(profile VehicleProfile, spec OperationSpec, f FieldSpec) FieldSpec {
	if f.DynamicBound == "temperature_c" {
		minV, maxV := profile.MinTempC, profile.MaxTempC
		if minV == 0 && maxV == 0 {
			minV, maxV = 16, 32
		}
		f.Min = &minV
		f.Max = &maxV
	}
	if spec.DynamicBounds != nil {
		f = spec.DynamicBounds(profile, f)
	}
	if len(f.Properties) > 0 {
		props := make([]FieldSpec, len(f.Properties))
		for i, child := range f.Properties {
			props[i] = applyFieldDynamicBounds(profile, spec, child)
		}
		f.Properties = props
	}
	return f
}

// OperationSpec defines one device.tool capability in the versioned functional map.
type OperationSpec struct {
	Operation         contract.Operation
	MCPTool           string
	DownstreamTool    string
	Title             string
	Category          string
	Description       string
	Aliases           []string
	Utterances        []string
	AntiExamples      []string
	GateKeywords      []string
	GatedTerms        []string
	DisabledByDefault bool
	MutuallyExclusive [][]string
	RequireAnyOf      [][]string
	MissingQuestions  map[string]string
	StatusSignals     []string
	Fields            []FieldSpec
	Guard             *GuardSpec
	// DynamicBounds optionally adjusts numeric bounds per vehicle profile programmatically.
	DynamicBounds func(p VehicleProfile, f FieldSpec) FieldSpec
	// QuestionFn optionally builds a custom Chinese follow-up question for missing fields.
	QuestionFn func(missing []string) string
}

// ParamsSchemaFor renders the exact LLM-facing JSON Schema for this operation and vehicle profile,
// automatically stripping sensitive runtime/identity fields.
func (spec OperationSpec) ParamsSchemaFor(profile VehicleProfile) json.RawMessage {
	schemaObj := buildObjectSchema(profile, spec, spec.Fields)
	raw, _ := json.Marshal(schemaObj)
	return raw
}

func buildObjectSchema(profile VehicleProfile, spec OperationSpec, fields []FieldSpec) map[string]any {
	props := make(map[string]any, len(fields))
	required := make([]string, 0, len(fields))
	for _, rawField := range fields {
		if rawField.isSensitive() {
			continue
		}
		f := applyFieldDynamicBounds(profile, spec, rawField)
		props[f.Name] = buildFieldSchema(profile, spec, f)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func buildFieldSchema(profile VehicleProfile, spec OperationSpec, f FieldSpec) map[string]any {
	prop := map[string]any{}
	if f.Description != "" {
		prop["description"] = f.Description
	}
	switch f.Type {
	case "string":
		prop["type"] = "string"
		if len(f.Enum) > 0 {
			prop["enum"] = f.Enum
		}
		if f.Min != nil {
			prop["x-minimum"] = *f.Min
		}
		if f.Max != nil {
			prop["x-maximum"] = *f.Max
		}
	case "number":
		prop["type"] = "number"
		if f.Min != nil {
			prop["minimum"] = *f.Min
		}
		if f.Max != nil {
			prop["maximum"] = *f.Max
		}
	case "integer":
		prop["type"] = "integer"
		if f.Min != nil {
			prop["minimum"] = int64(*f.Min)
		}
		if f.Max != nil {
			prop["maximum"] = int64(*f.Max)
		}
	case "boolean":
		prop["type"] = "boolean"
	case "array_string":
		prop["type"] = "array"
		items := map[string]any{"type": "string"}
		if len(f.Enum) > 0 {
			items["enum"] = f.Enum
		}
		prop["items"] = items
		if f.MinItems > 0 {
			prop["minItems"] = f.MinItems
		}
	case "array_integer":
		prop["type"] = "array"
		prop["items"] = map[string]any{"type": "integer"}
		if f.MinItems > 0 {
			prop["minItems"] = f.MinItems
		}
	case "object":
		if len(f.Properties) > 0 {
			return buildObjectSchema(profile, spec, f.Properties)
		}
		prop["type"] = "object"
	case "array_object":
		prop["type"] = "array"
		if len(f.Properties) > 0 {
			prop["items"] = buildObjectSchema(profile, spec, f.Properties)
		} else {
			prop["items"] = map[string]any{"type": "object"}
		}
		if f.MinItems > 0 {
			prop["minItems"] = f.MinItems
		}
	}
	return prop
}

// ValidationOutcome holds normalized parameters, missing required fields, and follow-up question.
type ValidationOutcome struct {
	Normalized    map[string]any
	MissingFields []string
	Question      string
}

// ValidateParams validates params against the same OperationSpec used to build ParamsSchema.
// When requireComplete is true, any missing required field or unmet RequireAnyOf returns INVALID_ARGUMENT.
func (spec OperationSpec) ValidateParams(profile VehicleProfile, params map[string]any, requireComplete bool) (ValidationOutcome, error) {
	if params == nil {
		params = map[string]any{}
	}
	normalized, missing, fieldByName, err := validateObjectFields(profile, spec, spec.Fields, params)
	if err != nil {
		return ValidationOutcome{}, err
	}

	// Enforce mutually exclusive parameter groups (e.g., temperature vs temperature_tune).
	for _, group := range spec.MutuallyExclusive {
		var present []string
		for _, name := range group {
			if _, ok := normalized[name]; ok {
				present = append(present, name)
			}
		}
		if len(present) > 1 {
			return ValidationOutcome{}, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameters [%s] are mutually exclusive for %s.%s", strings.Join(present, ", "), spec.Operation.Device, spec.Operation.Tool),
			}
		}
	}

	// Enforce require_any_of parameter groups (e.g., seat_heat_ctrl needs power or level).
	var unmetAnyOf [][]string
	for _, group := range spec.RequireAnyOf {
		satisfied := false
		for _, name := range group {
			if _, ok := normalized[name]; ok {
				satisfied = true
				break
			}
		}
		if !satisfied && len(group) > 0 {
			unmetAnyOf = append(unmetAnyOf, group)
			missing = append(missing, strings.Join(group, "|"))
		}
	}

	if requireComplete && len(missing) > 0 {
		if len(unmetAnyOf) > 0 && len(missing) == len(unmetAnyOf) {
			return ValidationOutcome{}, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("at least one of [%s] is required for %s.%s", strings.Join(unmetAnyOf[0], ", "), spec.Operation.Device, spec.Operation.Tool),
			}
		}
		return ValidationOutcome{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: fmt.Sprintf("missing required parameters %s for %s.%s", strings.Join(missing, ", "), spec.Operation.Device, spec.Operation.Tool),
		}
	}

	question := spec.buildMissingQuestion(missing, fieldByName)
	return ValidationOutcome{
		Normalized:    normalized,
		MissingFields: missing,
		Question:      question,
	}, nil
}

func (spec OperationSpec) buildMissingQuestion(missing []string, fieldByName map[string]FieldSpec) string {
	if len(missing) == 0 {
		return ""
	}
	if spec.QuestionFn != nil {
		return spec.QuestionFn(missing)
	}
	if len(spec.MissingQuestions) > 0 {
		if q, ok := spec.MissingQuestions[strings.Join(missing, "+")]; ok && q != "" {
			return q
		}
		if q, ok := spec.MissingQuestions[missing[0]]; ok && q != "" {
			return q
		}
	}
	if f, ok := fieldByName[missing[0]]; ok && f.Question != "" {
		return f.Question
	}
	return "请补充参数：" + strings.Join(missing, "、")
}

func validateObjectFields(profile VehicleProfile, spec OperationSpec, fields []FieldSpec, params map[string]any) (map[string]any, []string, map[string]FieldSpec, error) {
	fieldByName := make(map[string]FieldSpec, len(fields))
	for _, rawField := range fields {
		if rawField.isSensitive() {
			continue
		}
		f := applyFieldDynamicBounds(profile, spec, rawField)
		fieldByName[f.Name] = f
	}

	normalized := make(map[string]any, len(params))
	for k, v := range params {
		if IsSensitiveFieldName(k) {
			return nil, nil, nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("sensitive parameter %q is server-managed and cannot be supplied", k),
			}
		}
		f, ok := fieldByName[k]
		if !ok {
			return nil, nil, nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("unknown parameter %q for %s.%s", k, spec.Operation.Device, spec.Operation.Tool),
			}
		}
		if v == nil {
			continue
		}
		val, err := validateFieldValue(profile, spec, f, v)
		if err != nil {
			return nil, nil, nil, err
		}
		normalized[k] = val
	}

	var missing []string
	for _, f := range fields {
		if f.isSensitive() || !f.Required {
			continue
		}
		if _, ok := normalized[f.Name]; !ok {
			missing = append(missing, f.Name)
		}
	}
	return normalized, missing, fieldByName, nil
}

func validateFieldValue(profile VehicleProfile, spec OperationSpec, f FieldSpec, raw any) (any, error) {
	switch f.Type {
	case "string":
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be a non-empty string", f.Name),
			}
		}
		s = strings.TrimSpace(s)
		if len(f.Enum) > 0 && !containsString(f.Enum, s) {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q value %q not in allowed set [%s]", f.Name, s, strings.Join(f.Enum, ", ")),
			}
		}
		if f.Min != nil || f.Max != nil {
			num, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(num) || math.IsInf(num, 0) {
				return nil, &contract.BusinessError{
					Code:    contract.InvalidArgument,
					Message: fmt.Sprintf("parameter %q value %q must be a valid numeric string", f.Name, s),
				}
			}
			if f.Min != nil && num < *f.Min {
				return nil, &contract.BusinessError{
					Code:    contract.InvalidArgument,
					Message: fmt.Sprintf("parameter %q=%.1f below minimum %.1f", f.Name, num, *f.Min),
				}
			}
			if f.Max != nil && num > *f.Max {
				return nil, &contract.BusinessError{
					Code:    contract.InvalidArgument,
					Message: fmt.Sprintf("parameter %q=%.1f exceeds maximum %.1f", f.Name, num, *f.Max),
				}
			}
		}
		return s, nil

	case "number":
		num, ok := toFloat64(raw)
		if !ok || math.IsNaN(num) || math.IsInf(num, 0) {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be a finite number", f.Name),
			}
		}
		if f.Min != nil && num < *f.Min {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q=%.1f below minimum %.1f", f.Name, num, *f.Min),
			}
		}
		if f.Max != nil && num > *f.Max {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q=%.1f exceeds maximum %.1f", f.Name, num, *f.Max),
			}
		}
		return num, nil

	case "integer":
		iv, ok := toInt64(raw)
		if !ok {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be an integer", f.Name),
			}
		}
		if f.Min != nil && float64(iv) < *f.Min {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q=%d below minimum %.0f", f.Name, iv, *f.Min),
			}
		}
		if f.Max != nil && float64(iv) > *f.Max {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q=%d exceeds maximum %.0f", f.Name, iv, *f.Max),
			}
		}
		return iv, nil

	case "boolean":
		b, ok := raw.(bool)
		if !ok {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be a boolean", f.Name),
			}
		}
		return b, nil

	case "array_string":
		list, ok := toStringSlice(raw)
		if !ok {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be an array of strings", f.Name),
			}
		}
		if len(list) < f.MinItems {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q requires at least %d item(s)", f.Name, f.MinItems),
			}
		}
		seen := make(map[string]bool, len(list))
		dedup := make([]string, 0, len(list))
		for _, item := range list {
			if len(f.Enum) > 0 && !containsString(f.Enum, item) {
				return nil, &contract.BusinessError{
					Code:    contract.InvalidArgument,
					Message: fmt.Sprintf("parameter %q item %q not in allowed set [%s]", f.Name, item, strings.Join(f.Enum, ", ")),
				}
			}
			if !seen[item] {
				seen[item] = true
				dedup = append(dedup, item)
			}
		}
		return dedup, nil

	case "array_integer":
		list, ok := toInt64Slice(raw)
		if !ok {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be an array of integers", f.Name),
			}
		}
		if len(list) < f.MinItems {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q requires at least %d item(s)", f.Name, f.MinItems),
			}
		}
		seen := make(map[int64]bool, len(list))
		dedup := make([]int64, 0, len(list))
		for _, item := range list {
			if !seen[item] {
				seen[item] = true
				dedup = append(dedup, item)
			}
		}
		return dedup, nil

	case "object":
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be an object", f.Name),
			}
		}
		if len(f.Properties) == 0 {
			out := make(map[string]any, len(obj))
			for k, v := range obj {
				if IsSensitiveFieldName(k) {
					return nil, &contract.BusinessError{
						Code:    contract.InvalidArgument,
						Message: fmt.Sprintf("sensitive nested parameter %q.%q is forbidden", f.Name, k),
					}
				}
				out[k] = v
			}
			return out, nil
		}
		norm, missing, _, err := validateObjectFields(profile, spec, f.Properties, obj)
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q missing required fields [%s]", f.Name, strings.Join(missing, ", ")),
			}
		}
		return norm, nil

	case "array_object":
		rawSlice, ok := raw.([]any)
		if !ok {
			if typed, ok2 := raw.([]map[string]any); ok2 {
				rawSlice = make([]any, len(typed))
				for i, item := range typed {
					rawSlice[i] = item
				}
				ok = true
			}
		}
		if !ok {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be an array of objects", f.Name),
			}
		}
		if len(rawSlice) < f.MinItems {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q requires at least %d item(s)", f.Name, f.MinItems),
			}
		}
		out := make([]map[string]any, 0, len(rawSlice))
		subField := FieldSpec{Name: f.Name, Type: "object", Properties: f.Properties}
		for _, item := range rawSlice {
			v, err := validateFieldValue(profile, spec, subField, item)
			if err != nil {
				return nil, err
			}
			out = append(out, v.(map[string]any))
		}
		return out, nil

	default:
		return raw, nil
	}
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0, false
		}
		return int64(n), true
	case float32:
		f := float64(n)
		if math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
			return 0, false
		}
		return int64(f), true
	case json.Number:
		if iv, err := n.Int64(); err == nil {
			return iv, true
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

func toStringSlice(v any) ([]string, bool) {
	switch s := v.(type) {
	case []string:
		return s, true
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			str, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, str)
		}
		return out, true
	default:
		return nil, false
	}
}

func toInt64Slice(v any) ([]int64, bool) {
	switch s := v.(type) {
	case []int64:
		return s, true
	case []int:
		out := make([]int64, len(s))
		for i, item := range s {
			out[i] = int64(item)
		}
		return out, true
	case []any:
		out := make([]int64, 0, len(s))
		for _, item := range s {
			iv, ok := toInt64(item)
			if !ok {
				return nil, false
			}
			out = append(out, iv)
		}
		return out, true
	default:
		return nil, false
	}
}

// Service provides capability lookup, BM25 recall, parameter validation,
// telemetry status queries, command confirmation, and manual search.
type Service struct {
	Version string

	mu        sync.RWMutex
	specs     map[contract.Operation]OperationSpec
	order     []contract.Operation
	profiles  map[string]VehicleProfile
	telemetry TelemetryProvider
	manuals   []ManualEntry
}

// NewService constructs a default versioned catalog populated from the embedded
// declarative specs/demo.json bundle and sample versioned vehicle manuals.
func NewService(telemetry TelemetryProvider) *Service {
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
	if err := s.LoadCatalogJSON(demoCatalogJSON); err != nil {
		panic(fmt.Sprintf("invalid embedded demo catalog: %v", err))
	}
	return s
}

func (s *Service) onCatalogChangedLocked() {
	// Hook for precomputed retrieval index invalidation (P0-2).
}

func (s *Service) RegisterOperation(spec OperationSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.specs[spec.Operation]; !exists {
		s.order = append(s.order, spec.Operation)
	}
	s.specs[spec.Operation] = spec
	s.onCatalogChangedLocked()
}

func (s *Service) SetVehicleProfile(profile VehicleProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if profile.MinTempC == 0 && profile.MaxTempC == 0 {
		profile.MinTempC, profile.MaxTempC = 16, 32
	}
	if profile.ManualVersion == "" {
		profile.ManualVersion = s.Version
	}
	if profile.Model == "" {
		profile.Model = "EV-SUV-2026"
	}
	s.profiles[profile.VehicleID] = profile
}

func (s *Service) Profile(vehicleID string) VehicleProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.profiles[vehicleID]; ok {
		return p
	}
	return DefaultProfile(vehicleID)
}

func isSpecEnabled(spec OperationSpec, profile VehicleProfile) bool {
	if profile.DisabledOps[spec.Operation] {
		return false
	}
	if profile.DisabledTools[spec.Operation.Tool] {
		return false
	}
	if spec.DownstreamTool != "" && profile.DisabledTools[spec.DownstreamTool] {
		return false
	}
	if spec.DisabledByDefault {
		if profile.EnabledTools[spec.Operation.Tool] {
			return true
		}
		if spec.DownstreamTool != "" && profile.EnabledTools[spec.DownstreamTool] {
			return true
		}
		return false
	}
	return true
}

// LookupOperation resolves an operation and checks vehicle-level availability.
func (s *Service) LookupOperation(vehicleID string, op contract.Operation) (OperationSpec, VehicleProfile, error) {
	s.mu.RLock()
	spec, ok := s.specs[op]
	profile, hasProfile := s.profiles[vehicleID]
	s.mu.RUnlock()
	if !hasProfile {
		profile = DefaultProfile(vehicleID)
	}
	if !ok || !isSpecEnabled(spec, profile) {
		return OperationSpec{}, profile, &contract.BusinessError{
			Code:    contract.CapabilityUnavailable,
			Message: fmt.Sprintf("operation %s.%s is unavailable for vehicle %s", op.Device, op.Tool, vehicleID),
		}
	}
	return spec, profile, nil
}

// LookupByDownstreamTool finds an enabled operation by its downstream MCP tool name.
func (s *Service) LookupByDownstreamTool(vehicleID, downstreamTool string) (OperationSpec, VehicleProfile, error) {
	s.mu.RLock()
	profile, hasProfile := s.profiles[vehicleID]
	if !hasProfile {
		profile = DefaultProfile(vehicleID)
	}
	for _, op := range s.order {
		sp := s.specs[op]
		if (sp.DownstreamTool == downstreamTool || sp.Operation.Tool == downstreamTool) && isSpecEnabled(sp, profile) {
			s.mu.RUnlock()
			return sp, profile, nil
		}
	}
	s.mu.RUnlock()
	return OperationSpec{}, profile, &contract.BusinessError{
		Code:    contract.CapabilityUnavailable,
		Message: fmt.Sprintf("downstream tool %q is unavailable for vehicle %s", downstreamTool, vehicleID),
	}
}

// AllSpecs returns a sorted copy of all registered operation specs (including disabled ones).
func (s *Service) AllSpecs() []OperationSpec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]OperationSpec, 0, len(s.order))
	for _, op := range s.order {
		out = append(out, s.specs[op])
	}
	return out
}

// SchemaFor returns the exact JSON Schema for an operation on a specific vehicle.
func (s *Service) SchemaFor(vehicleID string, op contract.Operation) (json.RawMessage, error) {
	spec, profile, err := s.LookupOperation(vehicleID, op)
	if err != nil {
		return nil, err
	}
	return spec.ParamsSchemaFor(profile), nil
}

// ValidateMutation validates parameters against the vehicle's operation schema.
func (s *Service) ValidateMutation(vehicleID string, op contract.Operation, params map[string]any, requireComplete bool) (ValidationOutcome, OperationSpec, error) {
	spec, profile, err := s.LookupOperation(vehicleID, op)
	if err != nil {
		return ValidationOutcome{}, OperationSpec{}, err
	}
	outcome, err := spec.ValidateParams(profile, params, requireComplete)
	if err != nil {
		return ValidationOutcome{}, spec, err
	}
	return outcome, spec, nil
}

// CapabilityInfo is a read-only view of one enabled vehicle capability and its
// vehicle-specific JSON Schema, exported for Web Demo catalog inspection.
type CapabilityInfo struct {
	Device         string          `json:"device"`
	Tool           string          `json:"tool"`
	MCPTool        string          `json:"mcp_tool"`
	DownstreamTool string          `json:"downstream_tool,omitempty"`
	Title          string          `json:"title,omitempty"`
	Category       string          `json:"category,omitempty"`
	Description    string          `json:"description"`
	Aliases        []string        `json:"aliases"`
	Utterances     []string        `json:"utterances"`
	ParamsSchema   json.RawMessage `json:"params_schema"`
}

// ListCapabilities returns all enabled capabilities and schemas for vehicleID.
func (s *Service) ListCapabilities(vehicleID string) []CapabilityInfo {
	s.mu.RLock()
	profile, ok := s.profiles[vehicleID]
	if !ok {
		profile = DefaultProfile(vehicleID)
	}
	specs := s.enabledSpecsLocked(profile)
	s.mu.RUnlock()

	out := make([]CapabilityInfo, 0, len(specs))
	for _, sp := range specs {
		aliases := make([]string, len(sp.Aliases))
		copy(aliases, sp.Aliases)
		utterances := make([]string, len(sp.Utterances))
		copy(utterances, sp.Utterances)
		out = append(out, CapabilityInfo{
			Device:         sp.Operation.Device,
			Tool:           sp.Operation.Tool,
			MCPTool:        sp.MCPTool,
			DownstreamTool: sp.DownstreamTool,
			Title:          sp.Title,
			Category:       sp.Category,
			Description:    sp.Description,
			Aliases:        aliases,
			Utterances:     utterances,
			ParamsSchema:   sp.ParamsSchemaFor(profile),
		})
	}
	return out
}

func (s *Service) enabledSpecsLocked(profile VehicleProfile) []OperationSpec {
	out := make([]OperationSpec, 0, len(s.order))
	for _, op := range s.order {
		sp := s.specs[op]
		if !isSpecEnabled(sp, profile) {
			continue
		}
		out = append(out, sp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Operation.Device != out[j].Operation.Device {
			return out[i].Operation.Device < out[j].Operation.Device
		}
		return out[i].Operation.Tool < out[j].Operation.Tool
	})
	return out
}

var _ contract.CapabilityFinder = (*Service)(nil)

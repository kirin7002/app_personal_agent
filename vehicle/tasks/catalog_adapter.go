package tasks

import (
	"encoding/json"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
)

// CatalogValidator adapts *catalog.Service to tasks.ParamValidator so H03 and H06
// share one schema source of truth.
type CatalogValidator struct {
	Catalog *catalog.Service
}

func NewCatalogValidator(cat *catalog.Service) *CatalogValidator {
	return &CatalogValidator{Catalog: cat}
}

func (v *CatalogValidator) SchemaFor(vehicleID string, op contract.Operation) (json.RawMessage, error) {
	return v.Catalog.SchemaFor(vehicleID, op)
}

func (v *CatalogValidator) ValidateTaskParams(vehicleID string, op contract.Operation, params map[string]any, requireComplete bool) (ValidationResult, error) {
	out, _, err := v.Catalog.ValidateMutation(vehicleID, op, params, requireComplete)
	if err != nil {
		return ValidationResult{}, err
	}
	return ValidationResult{
		Normalized:    out.Normalized,
		MissingFields: out.MissingFields,
		Question:      out.Question,
	}, nil
}

var _ ParamValidator = (*CatalogValidator)(nil)

package planner

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/contracts/references"
)

func TestValidatorAcceptsFrozenPlans(t *testing.T) {
	t.Parallel()
	for _, document := range []string{minimalPlanV1, completePlanV1} {
		draft := mustParsePlanDraft(t, document)
		request := validValidationRequest(draft)
		if issues := NewValidator().Validate(request); len(issues) != 0 {
			t.Fatalf("Validate() issues = %#v", issues)
		}
	}
}

func TestValidatorEmitsEveryStableIssueCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		code  ValidationIssueCode
		build func(*testing.T) ValidatePlanRequest
	}{
		{name: "goal required", code: ValidationIssueGoalRequired, build: mutateMinimal(func(d *PlanDraft) { d.Goal = " " })},
		{name: "Step count", code: ValidationIssueStepCountInvalid, build: func(t *testing.T) ValidatePlanRequest {
			return validValidationRequest(PlanDraft{Goal: "g", Steps: []StepDraft{}})
		}},
		{name: "sequence", code: ValidationIssueStepSequenceInvalid, build: mutateMinimal(func(d *PlanDraft) { d.Steps[0].Sequence = 2 })},
		{name: "Step type", code: ValidationIssueStepTypeInvalid, build: mutateMinimal(func(d *PlanDraft) { d.Steps[0].Type = "Unknown" })},
		{name: "Step name", code: ValidationIssueStepNameRequired, build: mutateMinimal(func(d *PlanDraft) { d.Steps[0].Name = " " })},
		{name: "final Verification", code: ValidationIssueFinalVerificationRequired, build: mutateMinimal(func(d *PlanDraft) { d.Steps[0].Type = contracts.StepTypeAnalysis })},
		{name: "OutputSchema", code: ValidationIssueOutputSchemaInvalid, build: mutateMinimal(func(d *PlanDraft) { d.Steps[0].OutputSchema = nil })},
		{name: "OutputSchema field count", code: ValidationIssueOutputSchemaFieldLimitExceeded, build: mutateMinimal(func(d *PlanDraft) {
			d.Steps[0].OutputSchema = makeOutputSchema(maxOutputFields + 1)
		})},
		{name: "OutputSchema field name", code: ValidationIssueOutputFieldNameTooLong, build: mutateMinimal(func(d *PlanDraft) {
			d.Steps[0].OutputSchema = contracts.OutputSchema{validOutputFieldName(maxOutputFieldNameBytes + 1): {Type: contracts.OutputValueTypeString}}
		})},
		{name: "Tool name required", code: ValidationIssueToolNameRequired, build: mutateComplete(func(d *PlanDraft) { d.Steps[0].ToolName = nil })},
		{name: "Tool name forbidden", code: ValidationIssueToolNameForbidden, build: mutateMinimal(func(d *PlanDraft) {
			name := contracts.ToolName("get_deployment")
			d.Steps[0].ToolName = &name
		})},
		{name: "Tool not found", code: ValidationIssueToolNotFound, build: mutateCompleteRequest(func(r *ValidatePlanRequest) { r.ToolSnapshot.Tools = nil })},
		{name: "Tool disabled", code: ValidationIssueToolDisabled, build: mutateCompleteRequest(func(r *ValidatePlanRequest) { r.ToolSnapshot.Tools[0].Enabled = false })},
		{name: "Tool not allowed", code: ValidationIssueToolNotAllowed, build: mutateCompleteRequest(func(r *ValidatePlanRequest) { r.AllowedTools = []string{} })},
		{name: "Tool input", code: ValidationIssueToolInputInvalid, build: mutateComplete(func(d *PlanDraft) {
			d.Steps[0].Input = mustStepInput(tPlaceholder{}, `{"cluster":"primary"}`)
		})},
		{name: "reference syntax", code: ValidationIssueReferenceSyntaxInvalid, build: referenceValidationRequest("step.output.payload.extra", contracts.OutputValueTypeObject)},
		{name: "first Step reference", code: ValidationIssueReferenceNotAllowedOnFirstStep, build: mutateMinimal(func(d *PlanDraft) {
			d.Steps[0].Input = mustStepInput(tPlaceholder{}, `{"criteria":"c","evidence":"step.output.payload"}`)
		})},
		{name: "reference field", code: ValidationIssueReferenceFieldNotFound, build: referenceValidationRequest("step.output.missing", contracts.OutputValueTypeObject)},
		{name: "reference type", code: ValidationIssueReferenceTypeMismatch, build: referenceValidationRequest("step.output.payload", contracts.OutputValueTypeString)},
		{name: "expression", code: ValidationIssueExpressionNotSupported, build: referenceValidationRequest("${step.output.payload}", contracts.OutputValueTypeObject)},
		{name: "reference count", code: ValidationIssueReferenceCountLimitExceeded, build: func(t *testing.T) ValidatePlanRequest {
			return bulkReferenceValidationRequest(t, references.MaxResolvedReferencesPerStep+1)
		}},
		{name: "non Tool input", code: ValidationIssueNonToolInputInvalid, build: mutateMinimal(func(d *PlanDraft) {
			d.Steps[0].Input = mustStepInput(tPlaceholder{}, `{"criteria":1,"evidence":{}}`)
		})},
		{name: "Plan Step limit", code: ValidationIssuePlanStepLimitExceeded, build: func(t *testing.T) ValidatePlanRequest {
			return validValidationRequest(PlanDraft{Goal: "g", Steps: verificationSteps(maxPlanSteps + 1)})
		}},
		{name: "Plan draft size", code: ValidationIssuePlanDraftTooLarge, build: largePlanValidationRequest},
		{name: "goal size", code: ValidationIssuePlanGoalTooLong, build: mutateMinimal(func(d *PlanDraft) { d.Goal = strings.Repeat("g", maxGoalBytes+1) })},
		{name: "Step name size", code: ValidationIssueStepNameTooLong, build: mutateMinimal(func(d *PlanDraft) { d.Steps[0].Name = strings.Repeat("n", maxStepNameBytes+1) })},
		{name: "Step input size", code: ValidationIssueStepInputTooLarge, build: mutateComplete(func(d *PlanDraft) {
			d.Steps[0].Input = mustStepInput(tPlaceholder{}, `{"cluster":"`+strings.Repeat("x", maxStepInputBytes)+`"}`)
		})},
		{name: "JSON depth", code: ValidationIssueJSONDepthExceeded, build: mutateComplete(func(d *PlanDraft) {
			d.Steps[0].Input = mustStepInput(tPlaceholder{}, nestedToolInput(13))
		})},
		{name: "object fields", code: ValidationIssueObjectFieldLimitExceeded, build: mutateComplete(func(d *PlanDraft) {
			d.Steps[0].Input = mustStepInput(tPlaceholder{}, objectWithFields(maxObjectFields+1))
		})},
		{name: "issue count", code: ValidationIssueValidationIssueLimitExceeded, build: issueLimitValidationRequest},
		{name: "sensitive content", code: ValidationIssueSensitiveContentDetected, build: mutateMinimal(func(d *PlanDraft) {
			d.Steps[0].Input = mustStepInput(tPlaceholder{}, `{"criteria":"Bearer secret-value","evidence":{}}`)
		})},
		{name: "unsafe persistable content", code: ValidationIssueUnsafePersistableContent, build: mutateMinimal(func(d *PlanDraft) {
			d.Steps[0].Name = "unsafe\nname"
		})},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := test.build(t)
			issues := NewValidator().Validate(request)
			if !containsValidationIssue(issues, test.code) {
				t.Fatalf("Validate() issues = %#v, want %s", issues, test.code)
			}
		})
	}
}

func TestValidatorEnforcesAdjacentReferenceAndTypeRules(t *testing.T) {
	t.Parallel()
	valid := referenceValidationRequest("step.output.payload", contracts.OutputValueTypeObject)(t)
	if issues := NewValidator().Validate(valid); len(issues) != 0 {
		t.Fatalf("valid adjacent reference issues = %#v", issues)
	}

	nonAdjacent := PlanDraft{Goal: "g", Steps: []StepDraft{
		modelDraftStep(1, "first", contracts.OutputSchema{"old": {Type: contracts.OutputValueTypeObject}}),
		modelDraftStep(2, "second", contracts.OutputSchema{"new": {Type: contracts.OutputValueTypeObject}}),
		verificationDraftStep(3, `{"criteria":"c","evidence":"step.output.old"}`),
	}}
	issues := NewValidator().Validate(validValidationRequest(nonAdjacent))
	assertContainsValidationIssue(t, issues, ValidationIssueReferenceFieldNotFound)
}

func TestValidatorEnforcesToolSchemaLiteralReferenceAndAllowlist(t *testing.T) {
	t.Parallel()
	request := bulkReferenceValidationRequest(t, references.MaxResolvedReferencesPerStep)
	if issues := NewValidator().Validate(request); len(issues) != 0 {
		t.Fatalf("256 legal references issues = %#v", issues)
	}

	request = validValidationRequest(mustParsePlanDraft(t, completePlanV1))
	request.Draft.Steps[0].Input = mustStepInput(t, `{"cluster":"primary","namespace":false,"name":"demo"}`)
	issues := NewValidator().Validate(request)
	assertContainsValidationIssue(t, issues, ValidationIssueToolInputInvalid)
}

func TestToolSchemaNumberUsesJSONTokenType(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
		valid bool
	}{
		{name: "integer token", value: `1`, valid: true},
		{name: "negative integer token", value: `-1`, valid: true},
		{name: "fraction token", value: `1.5`, valid: true},
		{name: "exponent token", value: `1e3`, valid: true},
		{name: "negative exponent token", value: `-2.5E-2`, valid: true},
		{name: "integer string", value: `"1"`, valid: false},
		{name: "fraction string", value: `"1.5"`, valid: false},
		{name: "boolean", value: `true`, valid: false},
		{name: "object", value: `{}`, valid: false},
		{name: "array", value: `[]`, valid: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := numberToolValidationRequest(t, test.value)
			issues := NewValidator().Validate(request)
			if got := !containsValidationIssue(issues, ValidationIssueToolInputInvalid); got != test.valid {
				t.Fatalf("number value %s valid = %t, issues = %#v", test.value, got, issues)
			}
		})
	}
}

func TestToolSchemaPrimitiveTypesDoNotCoerce(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		schemaType contracts.JSONSchemaType
		value      string
		valid      bool
	}{
		{name: "string", schemaType: contracts.JSONSchemaTypeString, value: `"1"`, valid: true},
		{name: "number is not string", schemaType: contracts.JSONSchemaTypeString, value: `1`, valid: false},
		{name: "boolean", schemaType: contracts.JSONSchemaTypeBoolean, value: `true`, valid: true},
		{name: "string is not boolean", schemaType: contracts.JSONSchemaTypeBoolean, value: `"true"`, valid: false},
		{name: "integer", schemaType: contracts.JSONSchemaTypeInteger, value: `-42`, valid: true},
		{name: "fraction is not integer", schemaType: contracts.JSONSchemaTypeInteger, value: `42.0`, valid: false},
		{name: "exponent is not integer", schemaType: contracts.JSONSchemaTypeInteger, value: `42e0`, valid: false},
		{name: "string is not integer", schemaType: contracts.JSONSchemaTypeInteger, value: `"42"`, valid: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := matchesJSONSchemaPrimitive(json.RawMessage(test.value), test.schemaType); got != test.valid {
				t.Fatalf("matchesJSONSchemaPrimitive(%s, %s) = %t, want %t", test.value, test.schemaType, got, test.valid)
			}
		})
	}
}

func TestToolSchemaNumberAcceptsIntegerReference(t *testing.T) {
	t.Parallel()
	request := integerReferenceToNumberValidationRequest(t)
	if issues := NewValidator().Validate(request); len(issues) != 0 {
		t.Fatalf("integer reference to number issues = %#v", issues)
	}
}

func TestValidatorUsesStableOrderingAndIssueLimit(t *testing.T) {
	t.Parallel()
	request := issueLimitValidationRequest(t)
	first := NewValidator().Validate(request)
	second := NewValidator().Validate(request)
	if string(mustJSON(t, first)) != string(mustJSON(t, second)) {
		t.Fatalf("Validate() ordering changed:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if len(first) != maxValidationIssues {
		t.Fatalf("issue count = %d, want %d", len(first), maxValidationIssues)
	}
	if first[len(first)-1].Code != ValidationIssueValidationIssueLimitExceeded {
		t.Fatalf("last issue = %#v", first[len(first)-1])
	}
	for _, issue := range first {
		if !issue.Code.Valid() || issue.Summary == "" {
			t.Fatalf("invalid issue = %#v", issue)
		}
	}
}

func TestValidatorSanitizesDynamicIssuePaths(t *testing.T) {
	t.Parallel()
	draft := mustParsePlanDraft(t, minimalPlanV1)
	draft.Steps[0].OutputSchema = contracts.OutputSchema{
		"database_password": {Type: contracts.OutputValueType("invalid")},
	}
	issues := NewValidator().Validate(validValidationRequest(draft))
	assertContainsValidationIssue(t, issues, ValidationIssueOutputSchemaInvalid)
	for _, issue := range issues {
		if strings.Contains(issue.Path, "database_password") || strings.Contains(issue.Summary, "database_password") {
			t.Fatalf("ValidationIssue leaked dynamic field name: %#v", issue)
		}
	}
}

func TestExpressionIssueRetainsSharedSyntaxCompatibility(t *testing.T) {
	t.Parallel()
	request := referenceValidationRequest("coalesce (step.output.payload)", contracts.OutputValueTypeObject)(t)
	issues := NewValidator().Validate(request)
	assertContainsValidationIssue(t, issues, ValidationIssueExpressionNotSupported)
}

type inputTesting interface {
	Helper()
	Fatalf(string, ...any)
}

type tPlaceholder struct{}

func (tPlaceholder) Helper()                           {}
func (tPlaceholder) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }

func mustStepInput(t inputTesting, document string) StepInput {
	t.Helper()
	input, err := newStepInput([]byte(document))
	if err != nil {
		t.Fatalf("newStepInput(%s): %v", document, err)
	}
	return input
}

func mustParsePlanDraft(t *testing.T, document string) PlanDraft {
	t.Helper()
	draft, err := NewParser().ParseV1([]byte(document))
	if err != nil {
		t.Fatalf("ParseV1(): %v", err)
	}
	return draft
}

func validValidationRequest(draft PlanDraft) ValidatePlanRequest {
	additionalProperties := false
	return ValidatePlanRequest{
		Draft:        draft,
		MaxSteps:     maxPlanSteps,
		AllowedTools: []string{"get_deployment"},
		ToolSnapshot: contracts.PlanningToolSnapshot{Tools: []contracts.PlanningToolSpec{{
			ToolName: "get_deployment",
			Enabled:  true,
			InputSchema: contracts.CanonicalJSONSchema{
				Type: contracts.JSONSchemaTypeObject,
				Properties: map[string]contracts.CanonicalJSONSchema{
					"cluster":   {Type: contracts.JSONSchemaTypeString},
					"namespace": {Type: contracts.JSONSchemaTypeString},
					"name":      {Type: contracts.JSONSchemaTypeString},
				},
				Required:             []string{"cluster", "name", "namespace"},
				AdditionalProperties: &additionalProperties,
			},
		}}},
	}
}

func mutateMinimal(mutate func(*PlanDraft)) func(*testing.T) ValidatePlanRequest {
	return func(t *testing.T) ValidatePlanRequest {
		draft := mustParsePlanDraft(t, minimalPlanV1)
		mutate(&draft)
		return validValidationRequest(draft)
	}
}

func mutateComplete(mutate func(*PlanDraft)) func(*testing.T) ValidatePlanRequest {
	return func(t *testing.T) ValidatePlanRequest {
		draft := mustParsePlanDraft(t, completePlanV1)
		mutate(&draft)
		return validValidationRequest(draft)
	}
}

func mutateCompleteRequest(mutate func(*ValidatePlanRequest)) func(*testing.T) ValidatePlanRequest {
	return func(t *testing.T) ValidatePlanRequest {
		request := validValidationRequest(mustParsePlanDraft(t, completePlanV1))
		mutate(&request)
		return request
	}
}

func referenceValidationRequest(value string, sourceType contracts.OutputValueType) func(*testing.T) ValidatePlanRequest {
	return func(t *testing.T) ValidatePlanRequest {
		draft := PlanDraft{Goal: "g", Steps: []StepDraft{
			modelDraftStep(1, "source", contracts.OutputSchema{"payload": {Type: sourceType}}),
			verificationDraftStep(2, fmt.Sprintf(`{"criteria":"c","evidence":%q}`, value)),
		}}
		return validValidationRequest(draft)
	}
}

func modelDraftStep(sequence uint32, name string, output contracts.OutputSchema) StepDraft {
	return StepDraft{
		Sequence:     sequence,
		Type:         contracts.StepTypeModelCall,
		Name:         name,
		Input:        mustStepInput(tPlaceholder{}, `{"prompt":"p"}`),
		OutputSchema: output,
	}
}

func verificationDraftStep(sequence uint32, input string) StepDraft {
	return StepDraft{
		Sequence: sequence,
		Type:     contracts.StepTypeVerification,
		Name:     "verify",
		Input:    mustStepInput(tPlaceholder{}, input),
		OutputSchema: contracts.OutputSchema{
			"verified": {Type: contracts.OutputValueTypeBoolean},
		},
	}
}

func bulkReferenceValidationRequest(t *testing.T, count int) ValidatePlanRequest {
	t.Helper()
	referencesJSON := make([]string, count)
	for index := range referencesJSON {
		referencesJSON[index] = `"step.output.payload"`
	}
	toolName := contracts.ToolName("bulk")
	itemSchema := contracts.CanonicalJSONSchema{Type: contracts.JSONSchemaTypeString}
	additionalProperties := false
	draft := PlanDraft{Goal: "g", Steps: []StepDraft{
		modelDraftStep(1, "source", contracts.OutputSchema{"payload": {Type: contracts.OutputValueTypeString}}),
		{
			Sequence:     2,
			Type:         contracts.StepTypeToolCall,
			Name:         "bulk",
			Input:        mustStepInput(t, `{"items":[`+strings.Join(referencesJSON, ",")+`]}`),
			OutputSchema: contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeObject}},
			ToolName:     &toolName,
		},
		verificationDraftStep(3, `{"criteria":"c","evidence":"step.output.result"}`),
	}}
	return ValidatePlanRequest{
		Draft: draft, MaxSteps: maxPlanSteps, AllowedTools: []string{"bulk"},
		ToolSnapshot: contracts.PlanningToolSnapshot{Tools: []contracts.PlanningToolSpec{{
			ToolName: "bulk", Enabled: true,
			InputSchema: contracts.CanonicalJSONSchema{
				Type: contracts.JSONSchemaTypeObject,
				Properties: map[string]contracts.CanonicalJSONSchema{
					"items": {Type: contracts.JSONSchemaTypeArray, Items: &itemSchema},
				},
				Required: []string{"items"}, AdditionalProperties: &additionalProperties,
			},
		}}},
	}
}

func numberToolValidationRequest(t *testing.T, value string) ValidatePlanRequest {
	t.Helper()
	toolName := contracts.ToolName("number_tool")
	additionalProperties := false
	draft := PlanDraft{Goal: "g", Steps: []StepDraft{
		{
			Sequence: 1,
			Type:     contracts.StepTypeToolCall,
			Name:     "number",
			Input:    mustStepInput(t, `{"value":`+value+`}`),
			OutputSchema: contracts.OutputSchema{
				"result": {Type: contracts.OutputValueTypeObject},
			},
			ToolName: &toolName,
		},
		verificationDraftStep(2, `{"criteria":"c","evidence":"step.output.result"}`),
	}}
	return ValidatePlanRequest{
		Draft: draft, MaxSteps: maxPlanSteps, AllowedTools: []string{"number_tool"},
		ToolSnapshot: contracts.PlanningToolSnapshot{Tools: []contracts.PlanningToolSpec{{
			ToolName: "number_tool", Enabled: true,
			InputSchema: contracts.CanonicalJSONSchema{
				Type: contracts.JSONSchemaTypeObject,
				Properties: map[string]contracts.CanonicalJSONSchema{
					"value": {Type: contracts.JSONSchemaTypeNumber},
				},
				Required: []string{"value"}, AdditionalProperties: &additionalProperties,
			},
		}}},
	}
}

func integerReferenceToNumberValidationRequest(t *testing.T) ValidatePlanRequest {
	t.Helper()
	toolName := contracts.ToolName("number_tool")
	additionalProperties := false
	draft := PlanDraft{Goal: "g", Steps: []StepDraft{
		modelDraftStep(1, "source", contracts.OutputSchema{"count": {Type: contracts.OutputValueTypeInteger}}),
		{
			Sequence: 2,
			Type:     contracts.StepTypeToolCall,
			Name:     "number",
			Input:    mustStepInput(t, `{"value":"step.output.count"}`),
			OutputSchema: contracts.OutputSchema{
				"result": {Type: contracts.OutputValueTypeObject},
			},
			ToolName: &toolName,
		},
		verificationDraftStep(3, `{"criteria":"c","evidence":"step.output.result"}`),
	}}
	return ValidatePlanRequest{
		Draft: draft, MaxSteps: maxPlanSteps, AllowedTools: []string{"number_tool"},
		ToolSnapshot: contracts.PlanningToolSnapshot{Tools: []contracts.PlanningToolSpec{{
			ToolName: "number_tool", Enabled: true,
			InputSchema: contracts.CanonicalJSONSchema{
				Type: contracts.JSONSchemaTypeObject,
				Properties: map[string]contracts.CanonicalJSONSchema{
					"value": {Type: contracts.JSONSchemaTypeNumber},
				},
				Required: []string{"value"}, AdditionalProperties: &additionalProperties,
			},
		}}},
	}
}

func largePlanValidationRequest(t *testing.T) ValidatePlanRequest {
	t.Helper()
	toolName := contracts.ToolName("large")
	steps := make([]StepDraft, 0, 10)
	for index := 1; index <= 9; index++ {
		steps = append(steps, StepDraft{
			Sequence: uint32(index), Type: contracts.StepTypeToolCall, Name: "large",
			Input:        mustStepInput(t, `{"payload":"`+strings.Repeat("x", maxStepInputBytes-64)+`"}`),
			OutputSchema: contracts.OutputSchema{"value": {Type: contracts.OutputValueTypeString}},
			ToolName:     &toolName,
		})
	}
	steps = append(steps, verificationDraftStep(10, `{"criteria":"c","evidence":{}}`))
	additionalProperties := false
	return ValidatePlanRequest{
		Draft: PlanDraft{Goal: "g", Steps: steps}, MaxSteps: maxPlanSteps, AllowedTools: []string{"large"},
		ToolSnapshot: contracts.PlanningToolSnapshot{Tools: []contracts.PlanningToolSpec{{
			ToolName: "large", Enabled: true,
			InputSchema: contracts.CanonicalJSONSchema{
				Type:       contracts.JSONSchemaTypeObject,
				Properties: map[string]contracts.CanonicalJSONSchema{"payload": {Type: contracts.JSONSchemaTypeString}},
				Required:   []string{"payload"}, AdditionalProperties: &additionalProperties,
			},
		}}},
	}
}

func issueLimitValidationRequest(t *testing.T) ValidatePlanRequest {
	t.Helper()
	steps := verificationSteps(maxPlanSteps)
	for index := range steps {
		steps[index].Name = ""
		steps[index].OutputSchema = nil
	}
	return validValidationRequest(PlanDraft{Goal: "", Steps: steps})
}

func verificationSteps(count int) []StepDraft {
	steps := make([]StepDraft, count)
	for index := range steps {
		steps[index] = verificationDraftStep(uint32(index+1), `{"criteria":"c","evidence":{}}`)
	}
	return steps
}

func makeOutputSchema(count int) contracts.OutputSchema {
	result := make(contracts.OutputSchema, count)
	for index := range count {
		result[fmt.Sprintf("field_%d", index)] = contracts.OutputFieldSchema{Type: contracts.OutputValueTypeString}
	}
	return result
}

func containsValidationIssue(issues []ValidationIssue, code ValidationIssueCode) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func assertContainsValidationIssue(t *testing.T, issues []ValidationIssue, code ValidationIssueCode) {
	t.Helper()
	if !containsValidationIssue(issues, code) {
		t.Fatalf("issues = %#v, want %s", issues, code)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	return encoded
}

package planner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestSafeResultProcessorAcceptsValidCandidateWithoutMutation(t *testing.T) {
	t.Parallel()

	draft := mustParsePlanDraft(t, completePlanV1)
	before, err := canonicalCandidateSummary(draft)
	if err != nil {
		t.Fatalf("canonicalCandidateSummary() before error = %v", err)
	}
	processor := NewSafeResultProcessor()
	if issues := processor.Validate(draft); len(issues) != 0 {
		t.Fatalf("Validate() issues = %#v", issues)
	}
	summary, issues, err := processor.SafeCandidateSummary(draft)
	if err != nil || len(issues) != 0 {
		t.Fatalf("SafeCandidateSummary() = (%q, %#v, %v)", summary, issues, err)
	}
	after, err := canonicalCandidateSummary(draft)
	if err != nil {
		t.Fatalf("canonicalCandidateSummary() after error = %v", err)
	}
	if !bytes.Equal(before, after) || !bytes.Equal(summary, before) {
		t.Fatal("Safe Result Processor modified the candidate")
	}
	if validatorIssues := NewValidator().Validate(validValidationRequest(draft)); len(validatorIssues) != 0 {
		t.Fatalf("return-time Validator gate issues = %#v", validatorIssues)
	}
}

func TestSafeResultProcessorRejectsInvalidUTF8AndControlCharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PlanDraft)
		path   string
	}{
		{name: "goal invalid UTF-8", mutate: func(draft *PlanDraft) {
			draft.Goal = string([]byte{'g', 0xff})
		}, path: "$.goal"},
		{name: "Step name newline", mutate: func(draft *PlanDraft) {
			draft.Steps[0].Name = "unsafe\nname"
		}, path: "$.steps[0].name"},
		{name: "Tool name NUL", mutate: func(draft *PlanDraft) {
			name := contracts.ToolName("tool\x00name")
			draft.Steps[0].ToolName = &name
		}, path: "$.steps[0].tool_name"},
		{name: "input value tab", mutate: func(draft *PlanDraft) {
			draft.Steps[0].Input = mustStepInput(t, `{"criteria":"unsafe\tvalue","evidence":{}}`)
		}, path: "$.steps[0].input.<field>"},
		{name: "input raw invalid UTF-8", mutate: func(draft *PlanDraft) {
			draft.Steps[0].Input = StepInput{encoded: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}}
		}, path: "$.steps[0].input"},
		{name: "OutputSchema key invalid UTF-8", mutate: func(draft *PlanDraft) {
			draft.Steps[0].OutputSchema = contracts.OutputSchema{
				string([]byte{'x', 0xff}): {Type: contracts.OutputValueTypeString},
			}
		}, path: "$.steps[0].output_schema.<field>"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := mustParsePlanDraft(t, minimalPlanV1)
			test.mutate(&draft)
			issues := NewSafeResultProcessor().Validate(draft)
			assertSafetyIssue(t, issues, ValidationIssueUnsafePersistableContent, test.path)
			assertSafeIssuesDoNotLeakCandidate(t, issues, draft)
			assertContainsValidationIssue(t,
				NewValidator().Validate(validValidationRequest(draft)),
				ValidationIssueUnsafePersistableContent,
			)
		})
	}
}

func TestSafeResultProcessorRejectsEverySensitiveKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"password", "passwd", "secret", "token", "api_key", "apikey", "private_key",
		"client_secret", "authorization", "PaSsWoRd",
	} {
		key := key
		t.Run(key, func(t *testing.T) {
			draft := mustParsePlanDraft(t, minimalPlanV1)
			draft.Steps[0].Input = mustStepInput(t,
				`{"criteria":"safe","evidence":{"`+key+`":"sensitive-value"}}`)
			issues := NewSafeResultProcessor().Validate(draft)
			assertSafetyIssue(t, issues, ValidationIssueSensitiveContentDetected, "$.steps[0].input.<field>.<field>")
			assertSafeIssuesDoNotLeakCandidate(t, issues, draft)
		})
	}

	draft := mustParsePlanDraft(t, minimalPlanV1)
	draft.Steps[0].OutputSchema = contracts.OutputSchema{
		"client_secret": {Type: contracts.OutputValueTypeString},
	}
	assertSafetyIssue(t, NewSafeResultProcessor().Validate(draft),
		ValidationIssueSensitiveContentDetected, "$.steps[0].output_schema.<field>")
}

func TestSafeResultProcessorScansEveryPersistableStringLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PlanDraft)
		path   string
	}{
		{name: "goal", mutate: func(draft *PlanDraft) {
			draft.Goal = "Bearer goal-token"
		}, path: "$.goal"},
		{name: "Step name", mutate: func(draft *PlanDraft) {
			draft.Steps[0].Name = "Basic c3RlcDpwYXNz"
		}, path: "$.steps[0].name"},
		{name: "Tool name", mutate: func(draft *PlanDraft) {
			name := contracts.ToolName("Authorization: tool-secret")
			draft.Steps[0].ToolName = &name
		}, path: "$.steps[0].tool_name"},
		{name: "nested input array", mutate: func(draft *PlanDraft) {
			draft.Steps[0].Input = mustStepInput(t,
				`{"criteria":"safe","evidence":{"items":["Bearer nested-token"]}}`)
		}, path: "$.steps[0].input.<field>.<field>[0]"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := mustParsePlanDraft(t, minimalPlanV1)
			test.mutate(&draft)
			assertSafetyIssue(t, NewSafeResultProcessor().Validate(draft),
				ValidationIssueSensitiveContentDetected, test.path)
		})
	}
}

func TestSafeResultProcessorPEMPrivateKeyMarkers(t *testing.T) {
	t.Parallel()

	for _, marker := range []string{
		"-----BEGIN PRIVATE KEY-----",
		"-----BEGIN ENCRYPTED PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN DSA PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"-----BEGIN PGP PRIVATE KEY BLOCK-----",
	} {
		marker := marker
		t.Run(strings.Trim(marker, "-"), func(t *testing.T) {
			assertCredentialString(t, marker+"\nredacted", true)
		})
	}
	for _, ordinary := range []string{
		"-----BEGIN PUBLIC KEY-----",
		"-----BEGIN CERTIFICATE-----",
		"BEGIN PRIVATE KEY",
	} {
		assertCredentialString(t, ordinary, false)
	}
}

func TestSafeResultProcessorAuthorizationBoundaries(t *testing.T) {
	t.Parallel()

	for _, credential := range []string{
		"Bearer abc.def-_~+/=",
		"prefix, bearer token-value; suffix",
		"Basic dXNlcjpwYXNz",
		"prefix: basic Zm9vOmJhcg==",
		"Authorization: sensitive-value",
		"prefix, authorization = Bearer-value",
	} {
		assertCredentialString(t, credential, true)
	}
	for _, ordinary := range []string{
		"Bearer", "Basic", "Authorization", "Authorization is required",
		"Bearerish token", "preBearer token", "Basic-auth overview", "Basic authentication",
		"Basic bm8tY29sb24=",
	} {
		assertCredentialString(t, ordinary, false)
	}
}

func TestUnsafeInitialAndRepairCandidatesShareOneProcessor(t *testing.T) {
	t.Parallel()

	processor := NewSafeResultProcessor()
	initial := mustParsePlanDraft(t, minimalPlanV1)
	initial.Steps[0].Input = mustStepInput(t, `{"criteria":"Bearer initial-token","evidence":{}}`)
	initialIssues := processor.Validate(initial)
	assertSafetyIssue(t, initialIssues, ValidationIssueSensitiveContentDetected, "$.steps[0].input.<field>")

	gate := NewSingleRepairGate(NewPromptBuilder())
	if decision := gate.Decide(initialIssues); decision != RepairDecisionRequired {
		t.Fatalf("initial decision = %s, want %s", decision, RepairDecisionRequired)
	}
	messages, _, err := gate.Build(RepairPromptRequest{
		InitialPromptRequest: promptTestRequest(), Candidate: &initial,
		Issues: validationRepairIssues(initialIssues),
	})
	if err != nil || len(messages) != 1 {
		t.Fatalf("initial unsafe candidate did not enter one Repair: messages=%d err=%v", len(messages), err)
	}
	if strings.Contains(messages[0].Content, "initial-token") {
		t.Fatal("Repair Prompt leaked the unsafe initial candidate")
	}

	repaired := mustParsePlanDraft(t, minimalPlanV1)
	repaired.Steps[0].Input = mustStepInput(t, `{"criteria":"Basic dXNlcjpyZXBhaXItc2VjcmV0","evidence":{}}`)
	repairIssues := processor.Validate(repaired)
	assertSafetyIssue(t, repairIssues, ValidationIssueSensitiveContentDetected, "$.steps[0].input.<field>")
	if decision := gate.Decide(repairIssues); decision != RepairDecisionPlanValidationFailed {
		t.Fatalf("Repair decision = %s, want %s", decision, RepairDecisionPlanValidationFailed)
	}
	_, _, err = gate.Build(RepairPromptRequest{
		InitialPromptRequest: promptTestRequest(), Candidate: &repaired,
		Issues: validationRepairIssues(repairIssues),
	})
	assertPromptError(t, err, PromptErrorRepairExhausted)
}

func TestSafeRepairedCandidatePassesReturnGate(t *testing.T) {
	t.Parallel()

	initial := mustParsePlanDraft(t, minimalPlanV1)
	initial.Steps[0].Input = mustStepInput(t, `{"criteria":"Bearer initial-token","evidence":{}}`)
	gate := NewSingleRepairGate(NewPromptBuilder())
	_, _, err := gate.Build(RepairPromptRequest{
		InitialPromptRequest: promptTestRequest(), Candidate: &initial,
		Issues: validationRepairIssues(NewSafeResultProcessor().Validate(initial)),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	repaired := mustParsePlanDraft(t, minimalPlanV1)
	if issues := NewSafeResultProcessor().Validate(repaired); len(issues) != 0 {
		t.Fatalf("safe Repair candidate issues = %#v", issues)
	}
	if issues := NewValidator().Validate(validValidationRequest(repaired)); len(issues) != 0 {
		t.Fatalf("safe Repair return gate issues = %#v", issues)
	}
	if decision := gate.Decide(nil); decision != RepairDecisionAccepted {
		t.Fatalf("safe Repair decision = %s, want %s", decision, RepairDecisionAccepted)
	}
}

func assertCredentialString(t *testing.T, value string, sensitive bool) {
	t.Helper()
	draft := mustParsePlanDraft(t, minimalPlanV1)
	draft.Steps[0].Input = mustStepInput(t,
		`{"criteria":`+mustJSONString(t, value)+`,"evidence":{}}`)
	issues := NewSafeResultProcessor().Validate(draft)
	found := containsValidationIssue(issues, ValidationIssueSensitiveContentDetected)
	if found != sensitive {
		t.Fatalf("credential classification for %q = %t, want %t; issues=%#v", value, found, sensitive, issues)
	}
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(encoded)
}

func assertSafetyIssue(t *testing.T, issues []ValidationIssue, code ValidationIssueCode, path string) {
	t.Helper()
	for _, issue := range issues {
		if issue.Code == code && issue.Path == path && issue.Summary == string(code) {
			return
		}
	}
	t.Fatalf("issues = %#v, want (%s, %s)", issues, code, path)
}

func assertSafeIssuesDoNotLeakCandidate(t *testing.T, issues []ValidationIssue, draft PlanDraft) {
	t.Helper()
	for _, issue := range issues {
		if !utf8.ValidString(issue.Path) || !utf8.ValidString(issue.Summary) ||
			strings.Contains(issue.Path, "sensitive-value") || strings.Contains(issue.Summary, "sensitive-value") {
			t.Fatalf("unsafe issue = %#v for draft %#v", issue, draft)
		}
	}
}

func validationRepairIssues(issues []ValidationIssue) []RepairIssue {
	result := make([]RepairIssue, len(issues))
	for index, issue := range issues {
		result[index] = RepairIssue{Code: string(issue.Code), Path: issue.Path, Summary: issue.Summary}
	}
	return result
}

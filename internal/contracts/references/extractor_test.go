package references

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestExtractorExtractsKeyAndIndexPathsInCanonicalOrder(t *testing.T) {
	t.Parallel()

	result, err := NewStepReferenceExtractor().Extract(validRequest(json.RawMessage(`{
		"z":"step.output.zed",
		"spec":{"containers":[{"image":"step.output.image"}]},
		"a":"step.output.alpha",
		"literal":"ordinary text containing step.output.alpha"
	}`)))
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	references := result.ResolvedReferences
	if len(references) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(references))
	}
	assertPath(t, references[0].TargetPath, "key:a")
	assertPath(t, references[1].TargetPath, "key:spec", "key:containers", "index:0", "key:image")
	assertPath(t, references[2].TargetPath, "key:z")
	if references[1].SourceStepID != "step-1" || references[1].SourceOutputField != "image" {
		t.Fatalf("nested reference = %#v", references[1])
	}
}

func TestExtractorValidatesAdjacentSourceStep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*ExtractRequest)
		target error
	}{
		{name: "missing source", mutate: func(request *ExtractRequest) { request.SourceStep = nil }, target: ErrSourceStep},
		{name: "not adjacent", mutate: func(request *ExtractRequest) { request.SourceStep.Sequence = 1; request.TargetStepSequence = 3 }, target: ErrSourceStep},
		{name: "runtime source not completed", mutate: func(request *ExtractRequest) { request.SourceStep.Status = contracts.StepStatusRunning }, target: ErrSourceStep},
		{name: "field absent from schema", mutate: func(request *ExtractRequest) { delete(request.SourceStep.OutputSchema, "alpha") }, target: ErrSourceOutput},
		{name: "field absent from safe output", mutate: func(request *ExtractRequest) { request.SourceStep.SafeOutput = json.RawMessage(`{"other":true}`) }, target: ErrSourceOutput},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := validRequest(json.RawMessage(`{"value":"step.output.alpha"}`))
			test.mutate(&request)
			if _, err := NewStepReferenceExtractor().Extract(request); !errors.Is(err, test.target) {
				t.Fatalf("Extract() error = %v, want errors.Is(%v)", err, test.target)
			}
		})
	}
}

func TestExtractorStaticModeReusesTraversalWithoutRuntimeOutput(t *testing.T) {
	t.Parallel()

	request := validRequest(json.RawMessage(`{"value":"step.output.alpha"}`))
	request.ValidatePersistedOutput = false
	request.SourceStep.StepID = ""
	request.SourceStep.Status = contracts.StepStatusPending
	request.SourceStep.SafeOutput = nil
	result, err := NewStepReferenceExtractor().Extract(request)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(result.StaticReferences) != 1 || result.StaticReferences[0].SourceOutputField != "alpha" ||
		result.ResolvedReferences != nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestExtractorRuntimeModeRejectsMissingPersistedStepID(t *testing.T) {
	t.Parallel()

	request := validRequest(json.RawMessage(`{"value":"step.output.alpha"}`))
	request.SourceStep.StepID = ""
	if _, err := NewStepReferenceExtractor().Extract(request); !errors.Is(err, ErrSourceStep) {
		t.Fatalf("Extract() error = %v, want ErrSourceStep", err)
	}
}

func TestExtractorRejectsInvalidReferenceSyntax(t *testing.T) {
	t.Parallel()

	invalid := []string{
		"step.output.",
		"step.output.alpha.more",
		"step.output.alpha suffix",
		`coalesce(step.output.alpha, "fallback")`,
		`coalesce (step.output.alpha, "fallback")`,
		`functions.coalesce (step.output.alpha, "fallback")`,
		`functions::coalesce (step.output.alpha, "fallback")`,
		`functions:coalesce (step.output.alpha, "fallback")`,
		`default(step.output.alpha, "fallback")`,
		`if(step.output.alpha == "ready", "yes", "no")`,
		`if step.output.alpha then ready`,
		`step.output.alpha ?? "fallback"`,
		"${step.output.alpha}",
		"${step.output.alpha:-fallback}",
		"${ value: step.output.alpha }",
		"prefix ${step.output.alpha}",
		"${step.output.alpha} suffix",
		"prefix ${step.output.alpha} suffix",
		"{{step.output.alpha}}",
		"{{ step.output.alpha | default: 'fallback' }}",
		"{{ value step.output.alpha }}",
		"prefix {{step.output.alpha}}",
		"{{step.output.alpha}} suffix",
		"prefix {{step.output.alpha}} suffix",
	}
	for _, value := range invalid {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			request := validRequest(json.RawMessage(fmt.Sprintf(`{"value":%q}`, value)))
			if _, err := NewStepReferenceExtractor().Extract(request); !errors.Is(err, ErrReferenceSyntax) {
				t.Fatalf("Extract() error = %v, want ErrReferenceSyntax", err)
			}
		})
	}

	literals := []string{
		"ordinary step.output.alpha text",
		"prefix-step.output.alpha",
		"step output is step.output.alpha",
		"documentation mentions step.outputs.alpha",
		"documentation mentions step.outputting.alpha",
		"literal step.output. is incomplete",
		"ordinary (step.output.alpha) text",
		"ordinary step.output.alpha text = literal",
		"ordinary step.output.alpha text ?? fallback",
		"ordinary step.output.alpha text && more text",
		"value = step.output.alpha",
		"ordinary ${template} text with step.output.alpha explanation",
		"ordinary {{template}} text with step.output.alpha explanation",
		"unclosed ${step.output.alpha is documentation",
		"unclosed {{step.output.alpha is documentation",
		"coalesce is only a word without a reference",
	}
	for _, value := range literals {
		request := validRequest(json.RawMessage(fmt.Sprintf(`{"value":%q}`, value)))
		result, err := NewStepReferenceExtractor().Extract(request)
		if err != nil || len(result.ResolvedReferences) != 0 {
			t.Fatalf("literal %q result=%#v error=%v", value, result, err)
		}
	}
}

func TestClassifyReferenceStringFourExclusiveClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		wantClass referenceStringClass
		wantField string
	}{
		{name: "complete legal reference", value: "step.output.alpha", wantClass: referenceStringLegal, wantField: "alpha"},
		{name: "reserved prefix invalid", value: "step.output.alpha.more", wantClass: referenceStringReservedPrefixInvalid},
		{name: "explicit expression", value: "namespace.fn (step.output.alpha)", wantClass: referenceStringExpression},
		{name: "embedded dollar template", value: "before ${step.output.alpha} after", wantClass: referenceStringExpression},
		{name: "embedded mustache template", value: "before {{step.output.alpha}} after", wantClass: referenceStringExpression},
		{name: "ordinary middle text", value: "value = step.output.alpha for display", wantClass: referenceStringPlainText},
		{name: "similar characters", value: "step.outputs.alpha", wantClass: referenceStringPlainText},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotClass, gotField := classifyReferenceString(test.value)
			if gotClass != test.wantClass || gotField != test.wantField {
				t.Fatalf("classifyReferenceString(%q) = (%d, %q), want (%d, %q)",
					test.value, gotClass, gotField, test.wantClass, test.wantField)
			}
		})
	}
}

func TestExpressionErrorRefinesSyntaxWithoutBreakingCompatibility(t *testing.T) {
	t.Parallel()
	request := validRequest(json.RawMessage(`{"value":"namespace.fn (step.output.alpha)"}`))
	_, err := NewStepReferenceExtractor().Extract(request)
	if !errors.Is(err, ErrExpressionNotSupported) {
		t.Fatalf("Extract() error = %v, want ErrExpressionNotSupported", err)
	}
	if !errors.Is(err, ErrReferenceSyntax) {
		t.Fatalf("Extract() error = %v, want ErrReferenceSyntax compatibility", err)
	}
}

func TestExtractorRejectsDuplicateTargetDeterministically(t *testing.T) {
	t.Parallel()

	request := validRequest(json.RawMessage(`{"value":"step.output.alpha","value":"step.output.alpha"}`))
	if _, err := NewStepReferenceExtractor().Extract(request); !errors.Is(err, ErrDuplicateTarget) {
		t.Fatalf("Extract() error = %v, want ErrDuplicateTarget", err)
	}
}

func TestExtractorReturnsStableIssueForReferenceLimit(t *testing.T) {
	t.Parallel()

	var input strings.Builder
	input.WriteByte('{')
	for index := 0; index <= MaxResolvedReferencesPerStep; index++ {
		if index != 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `%q:"step.output.alpha"`, fmt.Sprintf("field_%03d", index))
	}
	input.WriteByte('}')

	request := validRequest(json.RawMessage(input.String()))
	_, err := NewStepReferenceExtractor().Extract(request)
	var issue *IssueError
	if !errors.As(err, &issue) {
		t.Fatalf("Extract() error = %v, want IssueError", err)
	}
	if issue.Code != contracts.ReferenceIssueCodeCountLimitExceeded || !issue.Code.Valid() {
		t.Fatalf("issue.Code = %q", issue.Code)
	}
}

func TestExtractorAcceptsExactlyReferenceLimit(t *testing.T) {
	t.Parallel()

	request := validRequest(referenceObject(MaxResolvedReferencesPerStep))
	result, err := NewStepReferenceExtractor().Extract(request)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(result.ResolvedReferences) != MaxResolvedReferencesPerStep {
		t.Fatalf("len(result) = %d, want %d", len(result.ResolvedReferences), MaxResolvedReferencesPerStep)
	}
}

func TestExtractorSortsArrayIndicesNumerically(t *testing.T) {
	t.Parallel()

	values := make([]string, 11)
	for index := range values {
		values[index] = "literal"
	}
	values[2] = "step.output.alpha"
	values[10] = "step.output.zed"
	input, err := json.Marshal(map[string]any{"items": values})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	result, err := NewStepReferenceExtractor().Extract(validRequest(input))
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	references := result.ResolvedReferences
	if len(references) != 2 || *references[0].TargetPath[1].Index != 2 || *references[1].TargetPath[1].Index != 10 {
		t.Fatalf("result = %#v", result)
	}
}

func TestExtractorRejectsReferencePathOverLimit(t *testing.T) {
	t.Parallel()

	input := `"step.output.alpha"`
	for index := 0; index < MaxTargetPathDepth+1; index++ {
		input = `{"key":` + input + `}`
	}
	request := validRequest(json.RawMessage(input))
	if _, err := NewStepReferenceExtractor().Extract(request); !errors.Is(err, ErrReferencePath) {
		t.Fatalf("Extract() error = %v, want ErrReferencePath", err)
	}
}

func TestExtractorNoStepInputAlwaysReturnsNonNilEmptyArray(t *testing.T) {
	t.Parallel()

	extractor := NewStepReferenceExtractor()
	for _, action := range []contracts.CheckpointNextAction{
		contracts.CheckpointNextActionGeneratePlan,
		contracts.CheckpointNextActionFinalizeRun,
	} {
		mode, err := ActionModeForNextAction(action)
		if err != nil {
			t.Fatalf("ActionModeForNextAction(%q) error = %v", action, err)
		}
		result, err := extractor.Extract(ExtractRequest{
			ActionMode: mode,
			StepInput:  json.RawMessage(`not read`),
		})
		if err != nil {
			t.Fatalf("Extract(%q) error = %v", action, err)
		}
		if result.StaticReferences == nil || len(result.StaticReferences) != 0 || result.ResolvedReferences != nil {
			t.Fatalf("Extract(%q) = %#v, want non-nil empty array", action, result)
		}
		encoded, err := json.Marshal(result.StaticReferences)
		if err != nil || string(encoded) != "[]" {
			t.Fatalf("Marshal(result) = %s, %v", encoded, err)
		}
	}
}

func TestActionModeForNextActionCoversFrozenSet(t *testing.T) {
	t.Parallel()

	tests := map[contracts.CheckpointNextAction]contracts.ReferenceActionMode{
		contracts.CheckpointNextActionGeneratePlan:        contracts.ReferenceActionModeNoStepInput,
		contracts.CheckpointNextActionExecuteStep:         contracts.ReferenceActionModeTargetStepInput,
		contracts.CheckpointNextActionRequestApproval:     contracts.ReferenceActionModeTargetStepInput,
		contracts.CheckpointNextActionExecuteApprovedTool: contracts.ReferenceActionModeTargetStepInput,
		contracts.CheckpointNextActionFinalizeRun:         contracts.ReferenceActionModeNoStepInput,
	}
	for action, want := range tests {
		got, err := ActionModeForNextAction(action)
		if err != nil || got != want {
			t.Fatalf("ActionModeForNextAction(%q) = %q, %v; want %q", action, got, err, want)
		}
	}
	if _, err := ActionModeForNextAction("UNKNOWN"); !errors.Is(err, ErrInvalidActionMode) {
		t.Fatalf("ActionModeForNextAction(UNKNOWN) error = %v", err)
	}
}

func validRequest(input json.RawMessage) ExtractRequest {
	return ExtractRequest{
		ActionMode:              contracts.ReferenceActionModeTargetStepInput,
		StepInput:               input,
		TargetStepSequence:      2,
		ValidatePersistedOutput: true,
		SourceStep: &SourceStep{
			StepID:   "step-1",
			Sequence: 1,
			Status:   contracts.StepStatusCompleted,
			OutputSchema: contracts.OutputSchema{
				"alpha": {Type: contracts.OutputValueTypeString},
				"image": {Type: contracts.OutputValueTypeString},
				"zed":   {Type: contracts.OutputValueTypeString},
			},
			SafeOutput: json.RawMessage(`{"alpha":"a","image":"i","zed":"z"}`),
		},
	}
}

func referenceObject(count int) json.RawMessage {
	var input strings.Builder
	input.WriteByte('{')
	for index := 0; index < count; index++ {
		if index != 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `%q:"step.output.alpha"`, fmt.Sprintf("field_%03d", index))
	}
	input.WriteByte('}')
	return json.RawMessage(input.String())
}

func assertPath(t *testing.T, path []contracts.ReferencePathSegment, want ...string) {
	t.Helper()
	got := make([]string, 0, len(path))
	for _, segment := range path {
		switch segment.Kind {
		case contracts.ReferencePathSegmentKey:
			got = append(got, "key:"+*segment.Key)
		case contracts.ReferencePathSegmentIndex:
			got = append(got, fmt.Sprintf("index:%d", *segment.Index))
		default:
			got = append(got, "unknown")
		}
	}
	if strings.Join(got, "/") != strings.Join(want, "/") {
		t.Fatalf("path = %v, want %v", got, want)
	}
}

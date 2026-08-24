package planner

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const minimalPlanV1 = `{
  "goal": "验证目标工作负载处于预期状态",
  "steps": [
    {
      "sequence": 1,
      "type": "Verification",
      "name": "验证目标状态",
      "input": {
        "criteria": "目标工作负载满足用户要求",
        "evidence": {}
      },
      "output_schema": {
        "verified": {"type": "boolean"}
      }
    }
  ]
}`

const completePlanV1 = `{
  "goal": "检查Deployment并分析其运行状态",
  "steps": [
    {
      "sequence": 1,
      "type": "ToolCall",
      "name": "读取Deployment",
      "input": {"cluster":"primary","namespace":"default","name":"demo"},
      "output_schema": {"deployment":{"type":"object"}},
      "tool_name": "get_deployment"
    },
    {
      "sequence": 2,
      "type": "ModelCall",
      "name": "整理Deployment信息",
      "input": {"prompt":"提取后续分析需要的事实","context":"step.output.deployment"},
      "output_schema": {"analysis_context":{"type":"object"}}
    },
    {
      "sequence": 3,
      "type": "Analysis",
      "name": "分析运行状态",
      "input": {"instruction":"判断副本状态和可用性是否异常","evidence":"step.output.analysis_context"},
      "output_schema": {"verification_context":{"type":"object"}}
    },
    {
      "sequence": 4,
      "type": "Verification",
      "name": "验证分析结论",
      "input": {"criteria":"结论必须由已采集事实支持","evidence":"step.output.verification_context"},
      "output_schema": {"verified":{"type":"boolean"},"summary":{"type":"string"}}
    }
  ]
}`

func TestParserParsesFrozenV1Examples(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		document string
		steps    int
	}{
		{name: "minimal", document: minimalPlanV1, steps: 1},
		{name: "all Step types and reference placeholders", document: completePlanV1, steps: 4},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			draft, err := NewParser().ParseV1([]byte(test.document))
			if err != nil {
				t.Fatalf("ParseV1() error = %v", err)
			}
			if draft.Goal == "" || len(draft.Steps) != test.steps {
				t.Fatalf("ParseV1() = %#v", draft)
			}
			encoded, err := marshalPlanDraft(draft)
			if err != nil {
				t.Fatalf("marshal PlanDraft: %v", err)
			}
			roundTrip, err := NewParser().ParseV1(encoded)
			if err != nil || !reflect.DeepEqual(roundTrip, draft) {
				t.Fatalf("round trip = (%#v, %v), want %#v", roundTrip, err, draft)
			}
		})
	}
	if PlanSchemaVersionV1 != 1 {
		t.Fatalf("PlanSchemaVersionV1 = %d", PlanSchemaVersionV1)
	}
}

func TestParserRejectsUnknownAndDuplicateFieldsAtEveryProtocolLayer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		doc  string
		code ParseIssueCode
	}{
		{name: "top unknown", doc: `{"goal":"g","steps":[],"plan_id":"forbidden"}`, code: ParseIssueUnknownField},
		{name: "Step unknown", doc: oneStep(`"status":"Pending",`), code: ParseIssueUnknownField},
		{name: "Output descriptor unknown", doc: oneStepWithOutput(`{"verified":{"type":"boolean","nullable":false}}`), code: ParseIssueOutputSchemaInvalid},
		{name: "non Tool input unknown", doc: oneStepWithInput(`{"criteria":"c","evidence":{},"extra":true}`), code: ParseIssueNonToolInputInvalid},
		{name: "top duplicate", doc: `{"goal":"a","goal":"b","steps":[]}`, code: ParseIssueDuplicateJSONKey},
		{name: "Step duplicate", doc: strings.Replace(oneStep(""), `"name":"verify"`, `"name":"a","name":"b"`, 1), code: ParseIssueDuplicateJSONKey},
		{name: "input duplicate", doc: oneStepWithInput(`{"criteria":"a","criteria":"b","evidence":{}}`), code: ParseIssueDuplicateJSONKey},
		{name: "Output descriptor duplicate", doc: oneStepWithOutput(`{"verified":{"type":"boolean","type":"string"}}`), code: ParseIssueDuplicateJSONKey},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertParseCode(t, test.doc, test.code)
		})
	}
}

func TestParserEnforcesRequiredNullEmptyAndToolNameRules(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		doc  string
		code ParseIssueCode
	}{
		{name: "missing goal", doc: `{"steps":[]}`, code: ParseIssueRequiredFieldMissing},
		{name: "missing steps", doc: `{"goal":"g"}`, code: ParseIssueRequiredFieldMissing},
		{name: "missing Step field", doc: `{"goal":"g","steps":[{"sequence":1}]}`, code: ParseIssueRequiredFieldMissing},
		{name: "goal null", doc: `{"goal":null,"steps":[]}`, code: ParseIssueNullNotAllowed},
		{name: "steps null", doc: `{"goal":"g","steps":null}`, code: ParseIssueNullNotAllowed},
		{name: "input null", doc: oneStepWithInput(`null`), code: ParseIssueNullNotAllowed},
		{name: "goal empty", doc: `{"goal":"  ","steps":[]}`, code: ParseIssueGoalRequired},
		{name: "steps empty", doc: `{"goal":"g","steps":[]}`, code: ParseIssueStepCountInvalid},
		{name: "name empty", doc: strings.Replace(oneStep(""), `"name":"verify"`, `"name":"  "`, 1), code: ParseIssueStepNameRequired},
		{name: "Output empty", doc: oneStepWithOutput(`{}`), code: ParseIssueOutputSchemaInvalid},
		{name: "non Tool tool name", doc: oneStep(`"tool_name":"x",`), code: ParseIssueToolNameForbidden},
		{name: "non Tool null tool name", doc: oneStep(`"tool_name":null,`), code: ParseIssueToolNameForbidden},
		{name: "Tool name missing", doc: toolStep("", `{"value":null}`), code: ParseIssueToolNameRequired},
		{name: "Tool name null", doc: toolStep(`"tool_name":null,`, `{}`), code: ParseIssueToolNameRequired},
		{name: "Tool name empty", doc: toolStep(`"tool_name":" ",`, `{}`), code: ParseIssueToolNameRequired},
		{name: "non Tool nested null", doc: oneStepWithInput(`{"criteria":"c","evidence":{"value":null}}`), code: ParseIssueNonToolInputInvalid},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertParseCode(t, test.doc, test.code)
		})
	}

	if _, err := NewParser().ParseV1([]byte(toolStep(`"tool_name":"tool.read",`, `{"optional":null}`))); err != nil {
		t.Fatalf("Tool input nullable value was rejected before Tool Schema validation: %v", err)
	}
}

func TestParserRejectsWrongJSONTypesWithoutCoercion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		doc  string
		code ParseIssueCode
	}{
		{name: "top array", doc: `[]`, code: ParseIssueInvalidJSON},
		{name: "goal number", doc: `{"goal":1,"steps":[]}`, code: ParseIssueInvalidJSON},
		{name: "steps object", doc: `{"goal":"g","steps":{}}`, code: ParseIssueInvalidJSON},
		{name: "sequence string", doc: strings.Replace(oneStep(""), `"sequence":1`, `"sequence":"1"`, 1), code: ParseIssueStepSequenceInvalid},
		{name: "sequence decimal", doc: strings.Replace(oneStep(""), `"sequence":1`, `"sequence":1.0`, 1), code: ParseIssueStepSequenceInvalid},
		{name: "sequence zero", doc: strings.Replace(oneStep(""), `"sequence":1`, `"sequence":0`, 1), code: ParseIssueStepSequenceInvalid},
		{name: "type case", doc: strings.Replace(oneStep(""), `"Verification"`, `"verification"`, 1), code: ParseIssueStepTypeInvalid},
		{name: "name number", doc: strings.Replace(oneStep(""), `"name":"verify"`, `"name":1`, 1), code: ParseIssueInvalidJSON},
		{name: "input array", doc: oneStepWithInput(`[]`), code: ParseIssueNonToolInputInvalid},
		{name: "Output array", doc: oneStepWithOutput(`[]`), code: ParseIssueOutputSchemaInvalid},
		{name: "Output type unknown", doc: oneStepWithOutput(`{"value":{"type":"null"}}`), code: ParseIssueOutputSchemaInvalid},
		{name: "Output type nonstring", doc: oneStepWithOutput(`{"value":{"type":1}}`), code: ParseIssueOutputSchemaInvalid},
		{name: "Tool name number", doc: toolStep(`"tool_name":1,`, `{}`), code: ParseIssueToolNameRequired},
		{name: "ModelCall prompt number", doc: modelStep(`{"prompt":1}`), code: ParseIssueNonToolInputInvalid},
		{name: "Analysis evidence array", doc: analysisStep(`{"instruction":"i","evidence":[]}`), code: ParseIssueNonToolInputInvalid},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertParseCode(t, test.doc, test.code)
		})
	}
}

func TestParserEnforcesFrozenNonToolInputShapes(t *testing.T) {
	t.Parallel()
	for _, document := range []string{
		modelStep(`{"prompt":"p"}`),
		modelStep(`{"prompt":"p","context":{}}`),
		modelStep(`{"prompt":"step.output.text","context":"step.output.object"}`),
		analysisStep(`{"instruction":"i","evidence":{}}`),
		analysisStep(`{"instruction":"step.output.text","evidence":"step.output.object"}`),
		oneStepWithInput(`{"criteria":"static","evidence":{}}`),
		oneStepWithInput(`{"criteria":"static","evidence":"step.output.object"}`),
	} {
		if _, err := NewParser().ParseV1([]byte(document)); err != nil {
			t.Fatalf("valid non Tool input rejected: %v", err)
		}
	}

	for _, document := range []string{
		modelStep(`{}`),
		modelStep(`{"prompt":"p","extra":true}`),
		modelStep(`{"prompt":"p","context":1}`),
		modelStep(`{"prompt":"p","context":null}`),
		analysisStep(`{"instruction":"i"}`),
		analysisStep(`{"instruction":"","evidence":{}}`),
		analysisStep(`{"instruction":"i","evidence":false}`),
		oneStepWithInput(`{"evidence":{}}`),
		oneStepWithInput(`{"criteria":1,"evidence":{}}`),
		oneStepWithInput(`{"criteria":"c","evidence":1}`),
		oneStepWithInput(`{"criteria":"c","evidence":{},"extra":true}`),
	} {
		assertParseCode(t, document, ParseIssueNonToolInputInvalid)
	}
}

func TestParserEnforcesFrozenSizeAndCountLimits(t *testing.T) {
	t.Parallel()
	goalBoundary := strings.Repeat("g", maxGoalBytes)
	if _, err := NewParser().ParseV1([]byte(strings.Replace(minimalPlanV1, "验证目标工作负载处于预期状态", goalBoundary, 1))); err != nil {
		t.Fatalf("goal at boundary: %v", err)
	}
	assertParseCode(t, strings.Replace(minimalPlanV1, "验证目标工作负载处于预期状态", goalBoundary+"g", 1), ParseIssuePlanGoalTooLong)

	nameBoundary := strings.Repeat("n", maxStepNameBytes)
	if _, err := NewParser().ParseV1([]byte(strings.Replace(oneStep(""), "verify", nameBoundary, 1))); err != nil {
		t.Fatalf("name at boundary: %v", err)
	}
	assertParseCode(t, strings.Replace(oneStep(""), "verify", nameBoundary+"n", 1), ParseIssueStepNameTooLong)

	inputOverhead := len(`{"prompt":""}`)
	inputBoundary := `{"prompt":"` + strings.Repeat("p", maxStepInputBytes-inputOverhead) + `"}`
	if _, err := NewParser().ParseV1([]byte(modelStep(inputBoundary))); err != nil {
		t.Fatalf("input at boundary: %v", err)
	}
	assertParseCode(t, modelStep(`{"prompt":"`+strings.Repeat("p", maxStepInputBytes-inputOverhead+1)+`"}`), ParseIssueStepInputTooLarge)

	if _, err := NewParser().ParseV1([]byte(planWithSteps(maxPlanSteps))); err != nil {
		t.Fatalf("Step count at boundary: %v", err)
	}
	assertParseCode(t, planWithSteps(maxPlanSteps+1), ParseIssuePlanStepLimitExceeded)

	if _, err := NewParser().ParseV1([]byte(toolStep(`"tool_name":"tool.read",`, objectWithFields(maxObjectFields)))); err != nil {
		t.Fatalf("object field count at boundary: %v", err)
	}
	assertParseCode(t, toolStep(`"tool_name":"tool.read",`, objectWithFields(maxObjectFields+1)), ParseIssueObjectFieldLimitExceeded)

	if _, err := NewParser().ParseV1([]byte(oneStepWithOutput(outputWithFields(maxOutputFields)))); err != nil {
		t.Fatalf("Output field count at boundary: %v", err)
	}
	assertParseCode(t, oneStepWithOutput(outputWithFields(maxOutputFields+1)), ParseIssueOutputFieldLimitExceeded)
	if _, err := NewParser().ParseV1([]byte(oneStepWithOutput(`{"` + validOutputFieldName(maxOutputFieldNameBytes) + `":{"type":"string"}}`))); err != nil {
		t.Fatalf("Output field name at boundary: %v", err)
	}
	assertParseCode(t, oneStepWithOutput(`{"`+validOutputFieldName(maxOutputFieldNameBytes+1)+`":{"type":"string"}}`), ParseIssueOutputFieldNameTooLong)
	assertParseCode(t, oneStepWithOutput(`{"invalid-name":{"type":"string"}}`), ParseIssueOutputSchemaInvalid)

	largeSteps := make([]string, 9)
	for index := range largeSteps {
		largeSteps[index] = rawToolStep(index+1, `"tool_name":"tool.read",`, objectWithStringBytes(maxStepInputBytes))
	}
	assertParseCode(t, `{"goal":"g","steps":[`+strings.Join(largeSteps, ",")+`]}`, ParseIssuePlanDraftTooLarge)

	oversizedResponse := append([]byte(minimalPlanV1), bytesOf(' ', maxModelResponseBytes-len(minimalPlanV1)+1)...)
	assertParseCodeBytes(t, oversizedResponse, ParseIssueModelResponseTooLarge)
}

func TestParserEnforcesDepthSingleValueAndUTF8(t *testing.T) {
	t.Parallel()
	if _, err := NewParser().ParseV1([]byte(toolStep(`"tool_name":"tool.read",`, nestedToolInput(12)))); err != nil {
		t.Fatalf("depth 16 boundary: %v", err)
	}
	assertParseCode(t, toolStep(`"tool_name":"tool.read",`, nestedToolInput(13)), ParseIssueJSONDepthExceeded)
	for _, document := range []string{
		"```json\n" + minimalPlanV1 + "\n```",
		minimalPlanV1 + ` {}`,
		"explanation " + minimalPlanV1,
	} {
		assertParseCode(t, document, ParseIssueInvalidJSON)
	}
	invalidUTF8 := append([]byte(`{"goal":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`","steps":[]}`)...)
	assertParseCodeBytes(t, invalidUTF8, ParseIssueInvalidJSON)
}

func TestPlanDraftIsSeparateFromPersistentEntity(t *testing.T) {
	t.Parallel()
	draftType := reflect.TypeOf(PlanDraft{})
	for _, forbidden := range []string{"PlanID", "RunID", "CreatedAt", "Status", "RawResponse"} {
		if _, exists := draftType.FieldByName(forbidden); exists {
			t.Fatalf("PlanDraft contains persistence/transient field %q", forbidden)
		}
	}
	if reflect.TypeOf(PlanDraft{}) == reflect.TypeOf(Entity{}) {
		t.Fatal("PlanDraft and persistent Entity unexpectedly share one type")
	}
	input, err := newStepInput([]byte(`{"key":"value"}`))
	if err != nil {
		t.Fatal(err)
	}
	copyValue := input.JSON()
	copyValue[0] = '['
	if string(input.JSON()) != `{"key":"value"}` {
		t.Fatal("StepInput exposed mutable internal JSON")
	}
}

func TestParseIssueCodesAreClosedAndErrorsDoNotEchoValues(t *testing.T) {
	t.Parallel()
	for _, code := range []ParseIssueCode{
		ParseIssueInvalidJSON, ParseIssueDuplicateJSONKey, ParseIssueUnknownField,
		ParseIssueRequiredFieldMissing, ParseIssueNullNotAllowed, ParseIssueGoalRequired,
		ParseIssueStepCountInvalid, ParseIssueStepSequenceInvalid, ParseIssueStepTypeInvalid,
		ParseIssueStepNameRequired, ParseIssueOutputSchemaInvalid, ParseIssueOutputFieldLimitExceeded,
		ParseIssueOutputFieldNameTooLong, ParseIssueToolNameRequired, ParseIssueToolNameForbidden,
		ParseIssueToolInputInvalid, ParseIssueNonToolInputInvalid, ParseIssuePlanStepLimitExceeded,
		ParseIssuePlanDraftTooLarge, ParseIssuePlanGoalTooLong, ParseIssueStepNameTooLong,
		ParseIssueStepInputTooLarge, ParseIssueJSONDepthExceeded, ParseIssueObjectFieldLimitExceeded,
		ParseIssueModelResponseTooLarge,
	} {
		if !code.Valid() {
			t.Fatalf("ParseIssueCode %q is not valid", code)
		}
	}
	if ParseIssueCode("UNKNOWN").Valid() {
		t.Fatal("unknown ParseIssueCode is valid")
	}
	secret := "secret-value-must-not-leak"
	_, err := NewParser().ParseV1([]byte(`{"goal":"g","steps":[],"` + secret + `":true}`))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("safe parser error = %v", err)
	}
}

func TestParserSanitizesModelControlledJSONPathSegments(t *testing.T) {
	t.Parallel()
	const sensitiveField = "database_password"
	for _, test := range []struct {
		name     string
		document string
		code     ParseIssueCode
		path     string
	}{
		{
			name:     "top-level unknown key",
			document: `{"goal":"g","steps":[],"database_password":true}`,
			code:     ParseIssueUnknownField,
			path:     `$.<field>`,
		},
		{
			name:     "Step input unknown key",
			document: oneStepWithInput(`{"criteria":"c","evidence":{},"database_password":"secret"}`),
			code:     ParseIssueNonToolInputInvalid,
			path:     `$.steps[0].input.<field>`,
		},
		{
			name:     "OutputSchema dynamic key",
			document: oneStepWithOutput(`{"database_password":{"type":"unsupported"}}`),
			code:     ParseIssueOutputSchemaInvalid,
			path:     `$.steps[0].output_schema.<field>.type`,
		},
		{
			name:     "duplicate dynamic key",
			document: toolStep(`"tool_name":"tool.read",`, `{"database_password":1,"database_password":2}`),
			code:     ParseIssueDuplicateJSONKey,
			path:     `$.steps[0].input.<field>`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewParser().ParseV1([]byte(test.document))
			var parseErr *ParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("ParseV1() error = %#v, want ParseError", err)
			}
			if parseErr.Code != test.code || parseErr.Path != test.path {
				t.Fatalf("ParseV1() error = %#v, want (%s, %s)", parseErr, test.code, test.path)
			}
			if strings.Contains(parseErr.Path, sensitiveField) || strings.Contains(parseErr.Error(), sensitiveField) {
				t.Fatalf("ParseError leaked model-controlled field name: %#v", parseErr)
			}
		})
	}
}

func TestParserPreservesFixedProtocolPaths(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		document string
		path     string
	}{
		{name: "required goal", document: `{"steps":[]}`, path: `$.goal`},
		{name: "Step type", document: strings.Replace(oneStep(""), `"Verification"`, `"invalid"`, 1), path: `$.steps[0].type`},
		{name: "non Tool input contract field", document: oneStepWithInput(`{"criteria":1,"evidence":{}}`), path: `$.steps[0].input.criteria`},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewParser().ParseV1([]byte(test.document))
			var parseErr *ParseError
			if !errors.As(err, &parseErr) || parseErr.Path != test.path {
				t.Fatalf("ParseV1() error = %#v, want path %s", err, test.path)
			}
		})
	}
}

func assertParseCode(t *testing.T, document string, want ParseIssueCode) {
	t.Helper()
	assertParseCodeBytes(t, []byte(document), want)
}

func assertParseCodeBytes(t *testing.T, document []byte, want ParseIssueCode) {
	t.Helper()
	_, err := NewParser().ParseV1(document)
	var parseErr *ParseError
	if !errors.As(err, &parseErr) || parseErr.Code != want {
		t.Fatalf("ParseV1() error = %#v, want %s", err, want)
	}
}

func oneStep(extra string) string {
	return `{"goal":"g","steps":[{` + extra + `"sequence":1,"type":"Verification","name":"verify","input":{"criteria":"c","evidence":{}},"output_schema":{"verified":{"type":"boolean"}}}]}`
}

func oneStepWithInput(input string) string {
	return `{"goal":"g","steps":[{"sequence":1,"type":"Verification","name":"verify","input":` + input + `,"output_schema":{"verified":{"type":"boolean"}}}]}`
}

func oneStepWithOutput(output string) string {
	return `{"goal":"g","steps":[{"sequence":1,"type":"Verification","name":"verify","input":{"criteria":"c","evidence":{}},"output_schema":` + output + `}]}`
}

func toolStep(extra, input string) string {
	return `{"goal":"g","steps":[{` + extra + `"sequence":1,"type":"ToolCall","name":"tool","input":` + input + `,"output_schema":{"value":{"type":"string"}}}]}`
}

func rawToolStep(sequence int, extra, input string) string {
	return fmt.Sprintf(`{%s"sequence":%d,"type":"ToolCall","name":"tool","input":%s,"output_schema":{"value":{"type":"string"}}}`, extra, sequence, input)
}

func modelStep(input string) string {
	return `{"goal":"g","steps":[{"sequence":1,"type":"ModelCall","name":"model","input":` + input + `,"output_schema":{"value":{"type":"string"}}}]}`
}

func analysisStep(input string) string {
	return `{"goal":"g","steps":[{"sequence":1,"type":"Analysis","name":"analysis","input":` + input + `,"output_schema":{"value":{"type":"string"}}}]}`
}

func planWithSteps(count int) string {
	steps := make([]string, count)
	for index := range steps {
		steps[index] = fmt.Sprintf(`{"sequence":%d,"type":"Verification","name":"verify","input":{"criteria":"c","evidence":{}},"output_schema":{"value":{"type":"string"}}}`, index+1)
	}
	return `{"goal":"g","steps":[` + strings.Join(steps, ",") + `]}`
}

func objectWithFields(count int) string {
	fields := make([]string, count)
	for index := range fields {
		fields[index] = fmt.Sprintf(`"field_%d":true`, index)
	}
	return `{` + strings.Join(fields, ",") + `}`
}

func outputWithFields(count int) string {
	fields := make([]string, count)
	for index := range fields {
		fields[index] = fmt.Sprintf(`"field_%d":{"type":"string"}`, index)
	}
	return `{` + strings.Join(fields, ",") + `}`
}

func validOutputFieldName(bytes int) string {
	if bytes <= 0 {
		return ""
	}
	return "f" + strings.Repeat("x", bytes-1)
}

func objectWithStringBytes(total int) string {
	overhead := len(`{"value":""}`)
	return `{"value":"` + strings.Repeat("x", total-overhead) + `"}`
}

func nestedToolInput(levels int) string {
	value := "true"
	for range levels {
		value = `{"nested":` + value + `}`
	}
	return `{"payload":` + value + `}`
}

func bytesOf(value byte, count int) []byte {
	return []byte(strings.Repeat(string(value), count))
}

var _ json.Marshaler = StepInput{}

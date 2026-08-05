package business

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestParseLoadsAgentAndAppliesGenerationDefaults(t *testing.T) {
	t.Parallel()
	document := templateDocument(t)
	params := nestedMap(t, document, "agents", 0, "execution_config", "model", "generation_params")
	delete(params, "temperature")
	delete(params, "top_p")
	delete(params, "max_output_tokens")

	config := parseDocument(t, document)
	agent, ok := config.Lookup("agent-default")
	if !ok {
		t.Fatal("Lookup() did not find agent-default")
	}
	if agent.TaskTimeout != 30*time.Minute {
		t.Fatalf("TaskTimeout = %s, want 30m", agent.TaskTimeout)
	}
	if agent.Name != "AgentOps Default" || agent.Description == "" || agent.CatalogID != "kubernetes-default" {
		t.Fatalf("agent metadata = %+v, want configured name, description, and catalog", agent)
	}
	paramsValue := agent.ExecutionConfig.Model.GenerationParams
	if paramsValue.Temperature.String() != "0.2" || paramsValue.TopP.String() != "1" || paramsValue.MaxOutputTokens != 4096 {
		t.Fatalf("generation defaults = %+v, want 0.2/1/4096", paramsValue)
	}
}

func TestParseNormalizesUnorderedSets(t *testing.T) {
	t.Parallel()
	document := templateDocument(t)
	execution := nestedMap(t, document, "agents", 0, "execution_config")
	agent := execution["agent"].(map[string]any)
	agent["allowed_tools"] = []any{"k8s.get_deployment", "k8s.get_deployment"}
	planner := execution["planner"].(map[string]any)
	planner["allowed_step_types"] = []any{"Verification", "ToolCall", "Analysis", "ModelCall", "Analysis"}
	patch := execution["tool_framework"].(map[string]any)["patch_policy"].(map[string]any)
	patch["allowed_write_fields"] = []any{"replicas", "image", "replicas"}

	config := parseDocument(t, document)
	loaded, _ := config.Lookup("agent-default")
	if got := loaded.ExecutionConfig.Agent.AllowedTools; len(got) != 1 || got[0] != "k8s.get_deployment" {
		t.Fatalf("AllowedTools = %v, want sorted unique value", got)
	}
	wantSteps := []contracts.StepType{
		contracts.StepTypeAnalysis, contracts.StepTypeModelCall, contracts.StepTypeToolCall, contracts.StepTypeVerification,
	}
	if got := loaded.ExecutionConfig.Planner.AllowedStepTypes; !equalStepTypes(got, wantSteps) {
		t.Fatalf("AllowedStepTypes = %v, want %v", got, wantSteps)
	}
	writes := loaded.ExecutionConfig.ToolFramework.PatchPolicy.AllowedWriteFields
	if len(writes) != 2 || writes[0] != "image" || writes[1] != "replicas" {
		t.Fatalf("AllowedWriteFields = %v, want [image replicas]", writes)
	}
}

func TestParseRejectsUnknownMissingNullAndInfrastructureFields(t *testing.T) {
	t.Parallel()
	tests := map[string]func(map[string]any){
		"unknown root": func(document map[string]any) { document["unknown"] = true },
		"infrastructure field": func(document map[string]any) {
			document["postgresql"] = map[string]any{"dsn": "secret"}
		},
		"unknown execution field": func(document map[string]any) {
			nestedMap(t, document, "agents", 0, "execution_config")["unknown"] = true
		},
		"missing required": func(document map[string]any) {
			delete(nestedMap(t, document, "agents", 0, "execution_config", "agent"), "enabled")
		},
		"null required": func(document map[string]any) {
			nestedMap(t, document, "agents", 0, "execution_config", "agent")["allowed_tools"] = nil
		},
		"missing timeout": func(document map[string]any) {
			delete(document["agents"].([]any)[0].(map[string]any), "task_timeout")
		},
		"missing catalog": func(document map[string]any) {
			delete(document["agents"].([]any)[0].(map[string]any), "catalog_id")
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := templateDocument(t)
			mutate(document)
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if _, err := Parse(bytes.NewReader(data)); err == nil {
				t.Fatal("Parse() error = nil, want strict validation error")
			}
		})
	}
}

func TestParseRejectsInvalidBusinessConfiguration(t *testing.T) {
	t.Parallel()
	tests := map[string]func(map[string]any){
		"disabled timeout": func(document map[string]any) {
			document["agents"].([]any)[0].(map[string]any)["task_timeout"] = "0s"
		},
		"duplicate agent": func(document map[string]any) {
			agents := document["agents"].([]any)
			document["agents"] = append(agents, agents[0])
		},
		"empty instruction": func(document map[string]any) {
			nestedMap(t, document, "agents", 0, "execution_config", "agent")["system_instruction"] = ""
		},
		"bad generation range": func(document map[string]any) {
			nestedMap(t, document, "agents", 0, "execution_config", "model", "generation_params")["top_p"] = 0
		},
		"unknown allowed tool": func(document map[string]any) {
			nestedMap(t, document, "agents", 0, "execution_config", "agent")["allowed_tools"] = []any{"missing"}
		},
		"invalid schema keyword": func(document map[string]any) {
			tools := nestedMap(t, document, "agents", 0, "execution_config", "tool_framework")["tools"].([]any)
			tools[0].(map[string]any)["input_schema"].(map[string]any)["oneOf"] = []any{}
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := templateDocument(t)
			mutate(document)
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if _, err := Parse(bytes.NewReader(data)); err == nil {
				t.Fatal("Parse() error = nil, want business validation error")
			}
		})
	}
}

func TestParseRejectsEmptyAndMultipleDocuments(t *testing.T) {
	t.Parallel()
	template := templateBytes(t)
	for name, input := range map[string][]byte{
		"empty":    nil,
		"multiple": append(append([]byte{}, template...), template...),
	} {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(bytes.NewReader(input)); err == nil {
				t.Fatal("Parse() error = nil, want document error")
			}
		})
	}
}

func TestParseRejectsDuplicateJSONMembers(t *testing.T) {
	t.Parallel()
	data := templateBytes(t)
	data = bytes.Replace(
		data,
		[]byte(`"schema": "agentops.execution-config",`),
		[]byte(`"schema": "agentops.execution-config", "schema": "agentops.execution-config",`),
		1,
	)
	if _, err := Parse(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "duplicate member") {
		t.Fatalf("Parse() error = %v, want duplicate member error", err)
	}
}

func TestLoadRejectsRawInvalidUTF8(t *testing.T) {
	t.Parallel()
	data := templateBytes(t)
	index := bytes.Index(data, []byte("AgentOps Default"))
	if index < 0 {
		t.Fatal("business template does not contain AgentOps Default")
	}
	data[index] = 0xff
	path := filepath.Join(t.TempDir(), "business.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write invalid UTF-8 fixture: %v", err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("Load() error = %v, want invalid UTF-8 error", err)
	}
}

func TestLoadAndAgentsAreDeterministic(t *testing.T) {
	t.Parallel()
	config, err := Load("../../../configs/business.json")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	agents := config.Agents()
	if len(agents) != 1 || agents[0].ExecutionConfig.Agent.AgentID != "agent-default" {
		t.Fatalf("Agents() = %+v, want agent-default", agents)
	}
	if _, err := Load(" "); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("Load(empty) error = %v, want path error", err)
	}
}

func TestLookupReturnsFrozenConfigurationCopy(t *testing.T) {
	t.Parallel()
	config, err := Load("../../../configs/business.json")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	first, _ := config.Lookup("agent-default")
	first.ExecutionConfig.Agent.AllowedTools[0] = "changed"
	first.ExecutionConfig.ToolFramework.Tools[0].InputSchema.Properties["cluster"] = contracts.CanonicalJSONSchema{
		Type: contracts.JSONSchemaTypeNumber,
	}
	second, _ := config.Lookup("agent-default")
	if second.ExecutionConfig.Agent.AllowedTools[0] != "k8s.get_deployment" ||
		second.ExecutionConfig.ToolFramework.Tools[0].InputSchema.Properties["cluster"].Type != contracts.JSONSchemaTypeString {
		t.Fatal("Lookup() caller mutation changed frozen configuration")
	}
}

func templateDocument(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(templateBytes(t), &document); err != nil {
		t.Fatalf("json.Unmarshal(template) error = %v", err)
	}
	return document
}

func templateBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../configs/business.json")
	if err != nil {
		t.Fatalf("read business template: %v", err)
	}
	return data
}

func nestedMap(t *testing.T, root map[string]any, path ...any) map[string]any {
	t.Helper()
	var current any = root
	for _, part := range path {
		switch key := part.(type) {
		case string:
			current = current.(map[string]any)[key]
		case int:
			current = current.([]any)[key]
		default:
			t.Fatalf("unsupported fixture path component %T", part)
		}
	}
	return current.(map[string]any)
}

func parseDocument(t *testing.T, document map[string]any) Config {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	config, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return config
}

func equalStepTypes(left, right []contracts.StepType) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

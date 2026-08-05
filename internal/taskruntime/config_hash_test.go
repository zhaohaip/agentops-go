package taskruntime_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/config/business"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

const fixedExecutionConfigHash = "27f26b3d37da75facddaca5b9cbc23de91093977d75746fb5c53a0d92d257f43"

func TestExecutionConfigFixedVector(t *testing.T) {
	t.Parallel()
	config, fixture := fixedConfig(t)

	canonical, err := taskruntime.CanonicalExecutionConfigV1(config)
	if err != nil {
		t.Fatalf("CanonicalExecutionConfigV1() error = %v", err)
	}
	if !bytes.Equal(canonical, fixture) {
		t.Fatalf("canonical bytes differ from fixed fixture\ngot:  %s\nwant: %s", canonical, fixture)
	}
	hash, err := taskruntime.HashExecutionConfigV1(config)
	if err != nil {
		t.Fatalf("HashExecutionConfigV1() error = %v", err)
	}
	if hash != fixedExecutionConfigHash || !hash.Valid() {
		t.Fatalf("hash = %q, want %q", hash, fixedExecutionConfigHash)
	}
}

func TestExecutionConfigSemanticFieldsAffectHash(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*contracts.ExecutionConfigV1){
		"agent id":          func(config *contracts.ExecutionConfigV1) { config.Agent.AgentID = "agent-other" },
		"agent enabled":     func(config *contracts.ExecutionConfigV1) { config.Agent.Enabled = false },
		"agent instruction": func(config *contracts.ExecutionConfigV1) { config.Agent.SystemInstruction += " changed" },
		"agent tools":       func(config *contracts.ExecutionConfigV1) { config.Agent.AllowedTools = []contracts.ToolName{} },
		"agent max steps":   func(config *contracts.ExecutionConfigV1) { config.Agent.MaxSteps++ },
		"model":             func(config *contracts.ExecutionConfigV1) { config.Model.ModelName += "-v2" },
		"generation": func(config *contracts.ExecutionConfigV1) {
			config.Model.GenerationParams.Temperature = contracts.NewCanonicalDecimalV1(3, 1)
		},
		"json contract": func(config *contracts.ExecutionConfigV1) { config.JSON.MaxDepth++ },
		"safety":        func(config *contracts.ExecutionConfigV1) { config.Safety.SafeSummaryMaxBytes++ },
		"planner":       func(config *contracts.ExecutionConfigV1) { config.Planner.Limits.MaxPlanSteps++ },
		"step executor": func(config *contracts.ExecutionConfigV1) { config.StepExecutor.Limits.MaxStepOutputBytes++ },
		"tool enabled":  func(config *contracts.ExecutionConfigV1) { config.ToolFramework.Tools[0].Enabled = false },
		"tool schema": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].InputSchema.Description = "changed"
		},
		"tool description": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].Description += " changed"
		},
		"tool capability": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].CapabilityKind = contracts.ToolCapabilityK8sGetPod
		},
		"tool output schema": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].OutputSchema.Description = "changed"
		},
		"tool risk": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].RiskLevel = contracts.RiskLevelHigh
		},
		"tool read only": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].ReadOnly = false
		},
		"tool timeout": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].TimeoutMS++
		},
		"tool access": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.AccessPolicy.Clusters[0].Namespaces = append(config.ToolFramework.AccessPolicy.Clusters[0].Namespaces, "ops")
		},
		"tool result limits": func(config *contracts.ExecutionConfigV1) { config.ToolFramework.ResultLimits.PodPageLimit++ },
		"event policy":       func(config *contracts.ExecutionConfigV1) { config.ToolFramework.EventPolicy.CandidateBudgetBytes++ },
		"patch policy": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.PatchPolicy.ResourceVersionTestRequired = false
		},
		"checkpoint contract": func(config *contracts.ExecutionConfigV1) { config.Checkpoint.MaxTargetPathDepth++ },
		"approval policy":     func(config *contracts.ExecutionConfigV1) { config.Approval.FreezeResourceVersion = false },
	}
	base, _ := fixedConfig(t)
	baseHash := mustHash(t, base)
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config, _ := fixedConfig(t)
			mutate(&config)
			if got := mustHash(t, config); got == baseHash {
				t.Fatalf("semantic change did not affect hash %q", got)
			}
		})
	}
}

func TestExecutionConfigUnorderedSetsNormalizeDeterministically(t *testing.T) {
	t.Parallel()
	left, _ := fixedConfig(t)
	second := left.ToolFramework.Tools[0]
	second.Name = "a.read"
	second.Description = "A read tool."
	left.ToolFramework.Tools = append(left.ToolFramework.Tools, second, left.ToolFramework.Tools[0])
	left.Planner.AllowedStepTypes = []contracts.StepType{
		contracts.StepTypeVerification, contracts.StepTypeToolCall, contracts.StepTypeAnalysis,
		contracts.StepTypeModelCall, contracts.StepTypeAnalysis,
	}
	left.ToolFramework.PatchPolicy.AllowedWriteFields = []string{"replicas", "image", "replicas"}

	right, _ := fixedConfig(t)
	right.ToolFramework.Tools = append([]contracts.ToolDefinitionV1{second}, right.ToolFramework.Tools...)
	right.Planner.AllowedStepTypes = []contracts.StepType{
		contracts.StepTypeAnalysis, contracts.StepTypeModelCall, contracts.StepTypeToolCall, contracts.StepTypeVerification,
	}
	right.ToolFramework.PatchPolicy.AllowedWriteFields = []string{"image", "replicas"}

	if leftHash, rightHash := mustHash(t, left), mustHash(t, right); leftHash != rightHash {
		t.Fatalf("normalized hashes differ: left=%s right=%s", leftHash, rightHash)
	}
	canonical, err := taskruntime.CanonicalExecutionConfigV1(left)
	if err != nil {
		t.Fatalf("CanonicalExecutionConfigV1() error = %v", err)
	}
	if bytes.Contains(canonical, []byte(`"allowed_write_fields":["replicas"`)) {
		t.Fatalf("canonical set was not sorted: %s", canonical)
	}
}

func TestExecutionConfigPreservesBusinessUnicodeAndWhitespace(t *testing.T) {
	t.Parallel()
	config, _ := fixedConfig(t)
	config.Agent.SystemInstruction = "  before\u2028after  "
	canonical, err := taskruntime.CanonicalExecutionConfigV1(config)
	if err != nil {
		t.Fatalf("CanonicalExecutionConfigV1() error = %v", err)
	}
	if !bytes.Contains(canonical, []byte("  before\u2028after  ")) {
		t.Fatalf("canonical JSON changed Unicode or business whitespace: %s", canonical)
	}
	if bytes.Contains(canonical, []byte(`before\u2028after`)) {
		t.Fatalf("canonical JSON escaped a valid Unicode scalar: %s", canonical)
	}
}

func TestExecutionConfigRejectsInvalidUTF8FromTypedCallers(t *testing.T) {
	t.Parallel()
	invalid := string([]byte{0xff})
	tests := map[string]func(*contracts.ExecutionConfigV1){
		"scalar": func(config *contracts.ExecutionConfigV1) {
			config.Agent.SystemInstruction = invalid
		},
		"named scalar": func(config *contracts.ExecutionConfigV1) {
			config.Model.ModelName = invalid
		},
		"string collection": func(config *contracts.ExecutionConfigV1) {
			config.Agent.AllowedTools = []contracts.ToolName{contracts.ToolName(invalid)}
		},
		"enum collection": func(config *contracts.ExecutionConfigV1) {
			config.Planner.AllowedStepTypes = []contracts.StepType{contracts.StepType(invalid)}
		},
		"tool field": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].Description = invalid
		},
		"access policy collection": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.AccessPolicy.Clusters[0].Namespaces = []string{invalid}
		},
		"resource collection": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.AccessPolicy.Clusters[0].Resources[0].Verbs = []string{invalid}
		},
		"registry collection": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.AccessPolicy.ImageRegistryAllowlist = []string{invalid}
		},
		"event enum collection": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.EventPolicy.SortKeys = []contracts.EventSortKey{contracts.EventSortKey(invalid)}
		},
		"patch collection": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.PatchPolicy.AllowedWriteFields = []string{invalid}
		},
		"schema description": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].InputSchema.Description = invalid
		},
		"schema required": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].InputSchema.Required = []string{invalid}
		},
		"schema property key": func(config *contracts.ExecutionConfigV1) {
			schema := &config.ToolFramework.Tools[0].InputSchema
			schema.Properties[invalid] = contracts.CanonicalJSONSchema{Type: contracts.JSONSchemaTypeString}
		},
		"nested schema": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].InputSchema.Properties["cluster"] = contracts.CanonicalJSONSchema{
				Type:        contracts.JSONSchemaTypeString,
				Description: invalid,
			}
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config, _ := fixedConfig(t)
			mutate(&config)
			if _, err := taskruntime.NormalizeExecutionConfigV1(config); err == nil || !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("NormalizeExecutionConfigV1() error = %v, want invalid UTF-8 error", err)
			}
			if canonical, err := taskruntime.CanonicalExecutionConfigV1(config); err == nil || canonical != nil {
				t.Fatalf("CanonicalExecutionConfigV1() = %s, %v; want nil and error", canonical, err)
			}
			if hash, err := taskruntime.HashExecutionConfigV1(config); err == nil || hash != "" {
				t.Fatalf("HashExecutionConfigV1() = %q, %v; want empty hash and error", hash, err)
			}
		})
	}
}

func TestExecutionConfigRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*contracts.ExecutionConfigV1){
		"schema":          func(config *contracts.ExecutionConfigV1) { config.Schema = "other" },
		"null collection": func(config *contracts.ExecutionConfigV1) { config.Agent.AllowedTools = nil },
		"temperature": func(config *contracts.ExecutionConfigV1) {
			config.Model.GenerationParams.Temperature = contracts.NewCanonicalDecimalV1(21, 1)
		},
		"top p": func(config *contracts.ExecutionConfigV1) {
			config.Model.GenerationParams.TopP = contracts.NewCanonicalDecimalV1(0, 0)
		},
		"undefined allowed tool": func(config *contracts.ExecutionConfigV1) {
			config.Agent.AllowedTools = append(config.Agent.AllowedTools, "missing")
		},
		"duplicate tool": func(config *contracts.ExecutionConfigV1) {
			duplicate := config.ToolFramework.Tools[0]
			duplicate.Description = "conflicting definition"
			config.ToolFramework.Tools = append(config.ToolFramework.Tools, duplicate)
		},
		"invalid schema required": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.Tools[0].InputSchema.Required = append(config.ToolFramework.Tools[0].InputSchema.Required, "missing")
		},
		"replicas disabled range": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.AccessPolicy.ReplicasPolicy.Max = 1
		},
		"duplicate ordered sort key": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.EventPolicy.SortKeys = append(config.ToolFramework.EventPolicy.SortKeys, contracts.EventSortKeyUIDAsc)
		},
		"zero limit": func(config *contracts.ExecutionConfigV1) { config.Planner.Limits.MaxPlanSteps = 0 },
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config, _ := fixedConfig(t)
			mutate(&config)
			if _, err := taskruntime.HashExecutionConfigV1(config); err == nil {
				t.Fatal("HashExecutionConfigV1() error = nil, want validation error")
			}
		})
	}
}

func TestExecutionConfigRejectsUnsupportedNestedVersions(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*contracts.ExecutionConfigV1){
		"model contract": func(config *contracts.ExecutionConfigV1) {
			config.Model.ModelClientContractVersion = "model-client-v2"
		},
		"model generation schema": func(config *contracts.ExecutionConfigV1) {
			config.Model.GenerationParamsSchemaVersion = 2
		},
		"JSON canonicalization": func(config *contracts.ExecutionConfigV1) {
			config.JSON.CanonicalizationVersion = "agentops-json-v2"
		},
		"safety sanitization": func(config *contracts.ExecutionConfigV1) {
			config.Safety.SanitizationRuleVersion = "result-sanitization-v2"
		},
		"planner contract": func(config *contracts.ExecutionConfigV1) {
			config.Planner.ContractVersion = "planner-v2"
		},
		"planner plan schema": func(config *contracts.ExecutionConfigV1) {
			config.Planner.PlanSchemaVersion = 2
		},
		"planner non-tool input": func(config *contracts.ExecutionConfigV1) {
			config.Planner.NonToolInputContractVersion = "non-tool-input-v2"
		},
		"planner tool schema subset": func(config *contracts.ExecutionConfigV1) {
			config.Planner.ToolSchemaSubsetVersion = "tool-schema-subset-v2"
		},
		"planner repair policy": func(config *contracts.ExecutionConfigV1) {
			config.Planner.RepairPolicyVersion = "multi-repair-v2"
		},
		"step executor contract": func(config *contracts.ExecutionConfigV1) {
			config.StepExecutor.ContractVersion = "step-executor-v2"
		},
		"step input contract": func(config *contracts.ExecutionConfigV1) {
			config.StepExecutor.StepInputContractVersion = "step-input-v2"
		},
		"step reference protocol": func(config *contracts.ExecutionConfigV1) {
			config.StepExecutor.ReferenceProtocolVersion = "step-output-ref-v2"
		},
		"step reference action mode": func(config *contracts.ExecutionConfigV1) {
			config.StepExecutor.ReferenceActionModeVersion = "reference-action-mode-v2"
		},
		"step output schema": func(config *contracts.ExecutionConfigV1) {
			config.StepExecutor.OutputSchemaVersion = "output-schema-v2"
		},
		"tool framework contract": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.ContractVersion = "tool-framework-v2"
		},
		"tool framework result": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.ResultContractVersion = "tool-framework-result-v2"
		},
		"tool event policy": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.EventPolicy.Version = "unbounded-event-page-v2"
		},
		"tool patch policy": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.PatchPolicy.Version = "deployment-patch-v2"
		},
		"tool response classification": func(config *contracts.ExecutionConfigV1) {
			config.ToolFramework.PatchPolicy.ResponseClassificationVersion = "patch-final-status-v2"
		},
		"checkpoint contract": func(config *contracts.ExecutionConfigV1) {
			config.Checkpoint.ContractVersion = "checkpoint-v2"
		},
		"checkpoint runtime context": func(config *contracts.ExecutionConfigV1) {
			config.Checkpoint.RuntimeContextSchemaVersion = 2
		},
		"checkpoint reference protocol": func(config *contracts.ExecutionConfigV1) {
			config.Checkpoint.ResolvedReferenceProtocolVersion = "step-output-ref-v2"
		},
		"checkpoint action mode": func(config *contracts.ExecutionConfigV1) {
			config.Checkpoint.ActionModeVersion = "checkpoint-action-mode-v2"
		},
		"approval policy": func(config *contracts.ExecutionConfigV1) {
			config.Approval.PolicyVersion = "approval-policy-v2"
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config, _ := fixedConfig(t)
			mutate(&config)
			if _, err := taskruntime.NormalizeExecutionConfigV1(config); err == nil {
				t.Fatal("NormalizeExecutionConfigV1() error = nil, want unsupported version error")
			}
			if canonical, err := taskruntime.CanonicalExecutionConfigV1(config); err == nil || canonical != nil {
				t.Fatalf("CanonicalExecutionConfigV1() = %s, %v; want nil and error", canonical, err)
			}
			if hash, err := taskruntime.HashExecutionConfigV1(config); err == nil || hash != "" {
				t.Fatalf("HashExecutionConfigV1() = %q, %v; want empty hash and error", hash, err)
			}
		})
	}
}

func TestInfrastructureConfigurationDoesNotAffectExecutionHash(t *testing.T) {
	t.Parallel()
	config, _ := fixedConfig(t)
	first := infra.Config{Logger: infra.LoggerConfig{Level: "info"}, Shutdown: infra.ShutdownConfig{Timeout: time.Second}}
	second := infra.Config{Logger: infra.LoggerConfig{Level: "debug"}, Shutdown: infra.ShutdownConfig{Timeout: time.Minute}}
	if first.Logger == second.Logger || first.Shutdown == second.Shutdown {
		t.Fatal("infrastructure fixtures must differ")
	}
	firstHash := mustHash(t, config)
	secondHash := mustHash(t, config)
	if firstHash != secondHash {
		t.Fatalf("infrastructure-only change affected hash: %s != %s", firstHash, secondHash)
	}
}

func fixedConfig(t *testing.T) (contracts.ExecutionConfigV1, []byte) {
	t.Helper()
	data, err := os.ReadFile("../../configs/business.json")
	if err != nil {
		t.Fatalf("read fixed business config: %v", err)
	}
	var raw struct {
		Agents []struct {
			ExecutionConfig json.RawMessage `json:"execution_config"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || len(raw.Agents) != 1 {
		t.Fatalf("decode fixed business fixture: agents=%d error=%v", len(raw.Agents), err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw.Agents[0].ExecutionConfig); err != nil {
		t.Fatalf("compact fixed execution fixture: %v", err)
	}
	businessConfig, err := business.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("business.Parse() error = %v", err)
	}
	agent, ok := businessConfig.Lookup("agent-default")
	if !ok {
		t.Fatal("fixed agent missing")
	}
	return agent.ExecutionConfig, slices.Clone(compact.Bytes())
}

func mustHash(t *testing.T, config contracts.ExecutionConfigV1) contracts.ExecutionConfigHash {
	t.Helper()
	hash, err := taskruntime.HashExecutionConfigV1(config)
	if err != nil {
		t.Fatalf("HashExecutionConfigV1() error = %v", err)
	}
	return hash
}

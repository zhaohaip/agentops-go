package taskruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const (
	supportedModelClientContractVersion                = "model-client-v1"
	supportedGenerationParamsSchemaVersion      uint32 = 1
	supportedJSONCanonicalizationVersion               = "agentops-json-v1"
	supportedSanitizationRuleVersion                   = "result-sanitization-v1"
	supportedPlannerContractVersion                    = "planner-v1.3"
	supportedPlanSchemaVersion                  uint32 = 1
	supportedNonToolInputContractVersion               = "non-tool-input-v1"
	supportedToolSchemaSubsetVersion                   = "tool-schema-subset-v1"
	supportedRepairPolicyVersion                       = "single-repair-v1"
	supportedStepExecutorContractVersion               = "step-executor-v1"
	supportedStepInputContractVersion                  = "step-input-v1"
	supportedStepReferenceProtocolVersion              = "step-output-ref-v1"
	supportedReferenceActionModeVersion                = "reference-action-mode-v1"
	supportedOutputSchemaVersion                       = "output-schema-v1"
	supportedToolFrameworkContractVersion              = "tool-framework-v1"
	supportedToolFrameworkResultContractVersion        = "tool-framework-result-v1"
	supportedEventPolicyVersion                        = "bounded-event-page-v1"
	supportedPatchPolicyVersion                        = "deployment-patch-v1"
	supportedPatchResponseClassificationVersion        = "patch-final-status-v1"
	supportedCheckpointContractVersion                 = "checkpoint-v1.3"
	supportedRuntimeContextSchemaVersion        uint32 = 1
	supportedCheckpointReferenceProtocolVersion        = "step-output-ref-v1"
	supportedCheckpointActionModeVersion               = "checkpoint-action-mode-v1"
	supportedApprovalPolicyVersion                     = "approval-policy-v1"
)

type utf8Field struct {
	path  string
	value string
}

// NormalizeExecutionConfigV1 校验并规范化完整的执行语义配置。
func NormalizeExecutionConfigV1(input contracts.ExecutionConfigV1) (contracts.ExecutionConfigV1, error) {
	config := input
	if err := validateExecutionConfigUTF8(config); err != nil {
		return contracts.ExecutionConfigV1{}, err
	}
	if err := validateSupportedExecutionConfigVersions(config); err != nil {
		return contracts.ExecutionConfigV1{}, err
	}

	config.Agent.AllowedTools = sortedUnique(config.Agent.AllowedTools)
	config.Planner.AllowedStepTypes = sortedUnique(config.Planner.AllowedStepTypes)
	config.ToolFramework.Tools = slices.Clone(config.ToolFramework.Tools)
	for index := range config.ToolFramework.Tools {
		tool := &config.ToolFramework.Tools[index]
		var err error
		tool.InputSchema, err = normalizeJSONSchema(tool.InputSchema, "tool input_schema")
		if err != nil {
			return contracts.ExecutionConfigV1{}, fmt.Errorf("normalize execution config: tool %q: %w", tool.Name, err)
		}
		tool.OutputSchema, err = normalizeJSONSchema(tool.OutputSchema, "tool output_schema")
		if err != nil {
			return contracts.ExecutionConfigV1{}, fmt.Errorf("normalize execution config: tool %q: %w", tool.Name, err)
		}
	}
	slices.SortFunc(config.ToolFramework.Tools, func(left, right contracts.ToolDefinitionV1) int {
		return strings.Compare(string(left.Name), string(right.Name))
	})
	compactedTools, err := compactTools(config.ToolFramework.Tools)
	if err != nil {
		return contracts.ExecutionConfigV1{}, err
	}
	config.ToolFramework.Tools = compactedTools

	config.ToolFramework.AccessPolicy.Clusters = slices.Clone(config.ToolFramework.AccessPolicy.Clusters)
	for clusterIndex := range config.ToolFramework.AccessPolicy.Clusters {
		cluster := &config.ToolFramework.AccessPolicy.Clusters[clusterIndex]
		cluster.Namespaces = sortedUnique(cluster.Namespaces)
		cluster.Resources = slices.Clone(cluster.Resources)
		for resourceIndex := range cluster.Resources {
			resource := &cluster.Resources[resourceIndex]
			resource.Verbs = sortedUnique(resource.Verbs)
			resource.WriteFields = sortedUnique(resource.WriteFields)
		}
		slices.SortFunc(cluster.Resources, func(left, right contracts.ResourcePolicyV1) int {
			return strings.Compare(left.Kind, right.Kind)
		})
		compactedResources, err := compactResources(cluster.Resources)
		if err != nil {
			return contracts.ExecutionConfigV1{}, fmt.Errorf("normalize execution config: cluster %q: %w", cluster.ClusterID, err)
		}
		cluster.Resources = compactedResources
	}
	slices.SortFunc(config.ToolFramework.AccessPolicy.Clusters, func(left, right contracts.ClusterPolicyV1) int {
		return strings.Compare(left.ClusterID, right.ClusterID)
	})
	compactedClusters, err := compactClusters(config.ToolFramework.AccessPolicy.Clusters)
	if err != nil {
		return contracts.ExecutionConfigV1{}, err
	}
	config.ToolFramework.AccessPolicy.Clusters = compactedClusters
	config.ToolFramework.AccessPolicy.ImageRegistryAllowlist = sortedUnique(
		config.ToolFramework.AccessPolicy.ImageRegistryAllowlist,
	)
	config.ToolFramework.PatchPolicy.AllowedWriteFields = sortedUnique(
		config.ToolFramework.PatchPolicy.AllowedWriteFields,
	)
	config.ToolFramework.EventPolicy.SortKeys = slices.Clone(config.ToolFramework.EventPolicy.SortKeys)

	if err := validateNormalizedExecutionConfig(config); err != nil {
		return contracts.ExecutionConfigV1{}, err
	}
	return config, nil
}

func validateExecutionConfigUTF8(config contracts.ExecutionConfigV1) error {
	fields := []utf8Field{
		{"schema", config.Schema},
		{"agent.agent_id", string(config.Agent.AgentID)},
		{"agent.system_instruction", config.Agent.SystemInstruction},
		{"model.model_name", config.Model.ModelName},
		{"model.response_format", config.Model.ResponseFormat},
		{"model.model_client_contract_version", config.Model.ModelClientContractVersion},
		{"json.canonicalization_version", config.JSON.CanonicalizationVersion},
		{"safety.sanitization_rule_version", config.Safety.SanitizationRuleVersion},
		{"planner.contract_version", config.Planner.ContractVersion},
		{"planner.non_tool_input_contract_version", config.Planner.NonToolInputContractVersion},
		{"planner.tool_schema_subset_version", config.Planner.ToolSchemaSubsetVersion},
		{"planner.repair_policy_version", config.Planner.RepairPolicyVersion},
		{"planner.final_step_type", string(config.Planner.FinalStepType)},
		{"step_executor.contract_version", config.StepExecutor.ContractVersion},
		{"step_executor.step_input_contract_version", config.StepExecutor.StepInputContractVersion},
		{"step_executor.reference_protocol_version", config.StepExecutor.ReferenceProtocolVersion},
		{"step_executor.reference_action_mode_version", config.StepExecutor.ReferenceActionModeVersion},
		{"step_executor.output_schema_version", config.StepExecutor.OutputSchemaVersion},
		{"tool_framework.contract_version", config.ToolFramework.ContractVersion},
		{"tool_framework.result_contract_version", config.ToolFramework.ResultContractVersion},
		{"tool_framework.event_policy.version", config.ToolFramework.EventPolicy.Version},
		{"tool_framework.patch_policy.version", config.ToolFramework.PatchPolicy.Version},
		{"tool_framework.patch_policy.response_classification_version", config.ToolFramework.PatchPolicy.ResponseClassificationVersion},
		{"checkpoint.contract_version", config.Checkpoint.ContractVersion},
		{"checkpoint.resolved_reference_protocol_version", config.Checkpoint.ResolvedReferenceProtocolVersion},
		{"checkpoint.action_mode_version", config.Checkpoint.ActionModeVersion},
		{"approval.policy_version", config.Approval.PolicyVersion},
		{"approval.required_risk_level", string(config.Approval.RequiredRiskLevel)},
	}
	for _, field := range fields {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("normalize execution config: %s must contain valid UTF-8", field.path)
		}
	}
	if err := validateUTF8Strings("agent.allowed_tools", config.Agent.AllowedTools); err != nil {
		return err
	}
	if err := validateUTF8Strings("planner.allowed_step_types", config.Planner.AllowedStepTypes); err != nil {
		return err
	}
	for index, tool := range config.ToolFramework.Tools {
		toolPath := fmt.Sprintf("tool_framework.tools[%d]", index)
		if err := validateUTF8Fields(
			utf8Field{toolPath + ".name", string(tool.Name)},
			utf8Field{toolPath + ".description", tool.Description},
			utf8Field{toolPath + ".capability_kind", string(tool.CapabilityKind)},
			utf8Field{toolPath + ".risk_level", string(tool.RiskLevel)},
		); err != nil {
			return err
		}
		if err := validateJSONSchemaUTF8(tool.InputSchema, toolPath+".input_schema"); err != nil {
			return err
		}
		if err := validateJSONSchemaUTF8(tool.OutputSchema, toolPath+".output_schema"); err != nil {
			return err
		}
	}
	for clusterIndex, cluster := range config.ToolFramework.AccessPolicy.Clusters {
		clusterPath := fmt.Sprintf("tool_framework.access_policy.clusters[%d]", clusterIndex)
		if !utf8.ValidString(cluster.ClusterID) {
			return fmt.Errorf("normalize execution config: %s.cluster_id must contain valid UTF-8", clusterPath)
		}
		if err := validateUTF8Strings(clusterPath+".namespaces", cluster.Namespaces); err != nil {
			return err
		}
		for resourceIndex, resource := range cluster.Resources {
			resourcePath := fmt.Sprintf("%s.resources[%d]", clusterPath, resourceIndex)
			if !utf8.ValidString(resource.Kind) {
				return fmt.Errorf("normalize execution config: %s.kind must contain valid UTF-8", resourcePath)
			}
			if err := validateUTF8Strings(resourcePath+".verbs", resource.Verbs); err != nil {
				return err
			}
			if err := validateUTF8Strings(resourcePath+".write_fields", resource.WriteFields); err != nil {
				return err
			}
		}
	}
	if err := validateUTF8Strings(
		"tool_framework.access_policy.image_registry_allowlist",
		config.ToolFramework.AccessPolicy.ImageRegistryAllowlist,
	); err != nil {
		return err
	}
	if err := validateUTF8Strings("tool_framework.event_policy.sort_keys", config.ToolFramework.EventPolicy.SortKeys); err != nil {
		return err
	}
	return validateUTF8Strings(
		"tool_framework.patch_policy.allowed_write_fields",
		config.ToolFramework.PatchPolicy.AllowedWriteFields,
	)
}

func validateUTF8Fields(fields ...utf8Field) error {
	for _, field := range fields {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("normalize execution config: %s must contain valid UTF-8", field.path)
		}
	}
	return nil
}

func validateUTF8Strings[S ~[]E, E ~string](path string, values S) error {
	for index, value := range values {
		if !utf8.ValidString(string(value)) {
			return fmt.Errorf("normalize execution config: %s[%d] must contain valid UTF-8", path, index)
		}
	}
	return nil
}

func validateJSONSchemaUTF8(schema contracts.CanonicalJSONSchema, path string) error {
	if err := validateUTF8Fields(
		utf8Field{path + ".type", string(schema.Type)},
		utf8Field{path + ".description", schema.Description},
	); err != nil {
		return err
	}
	if err := validateUTF8Strings(path+".required", schema.Required); err != nil {
		return err
	}
	for propertyName, propertySchema := range schema.Properties {
		if !utf8.ValidString(propertyName) {
			return fmt.Errorf("normalize execution config: %s.properties key must contain valid UTF-8", path)
		}
		if err := validateJSONSchemaUTF8(propertySchema, path+".properties."+propertyName); err != nil {
			return err
		}
	}
	if schema.Items != nil {
		return validateJSONSchemaUTF8(*schema.Items, path+".items")
	}
	return nil
}

// CanonicalExecutionConfigV1 返回完整 ExecutionConfigV1 的唯一规范化 JSON。
func CanonicalExecutionConfigV1(input contracts.ExecutionConfigV1) ([]byte, error) {
	config, err := NormalizeExecutionConfigV1(input)
	if err != nil {
		return nil, err
	}

	canonical := canonicalExecutionConfig{
		Schema:       config.Schema,
		Version:      config.Version,
		Agent:        config.Agent,
		Model:        config.Model,
		JSON:         config.JSON,
		Safety:       config.Safety,
		Planner:      config.Planner,
		StepExecutor: config.StepExecutor,
		ToolFramework: canonicalToolFramework{
			ContractVersion:       config.ToolFramework.ContractVersion,
			ResultContractVersion: config.ToolFramework.ResultContractVersion,
			Tools:                 make([]canonicalTool, 0, len(config.ToolFramework.Tools)),
			AccessPolicy:          config.ToolFramework.AccessPolicy,
			ResultLimits:          config.ToolFramework.ResultLimits,
			EventPolicy:           config.ToolFramework.EventPolicy,
			PatchPolicy:           config.ToolFramework.PatchPolicy,
		},
		Checkpoint: config.Checkpoint,
		Approval:   config.Approval,
	}
	for _, tool := range config.ToolFramework.Tools {
		inputSchema, err := marshalCanonicalSchema(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode execution config: tool %q input schema: %w", tool.Name, err)
		}
		outputSchema, err := marshalCanonicalSchema(tool.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode execution config: tool %q output schema: %w", tool.Name, err)
		}
		canonical.ToolFramework.Tools = append(canonical.ToolFramework.Tools, canonicalTool{
			Name:           tool.Name,
			Enabled:        tool.Enabled,
			Description:    tool.Description,
			CapabilityKind: tool.CapabilityKind,
			InputSchema:    inputSchema,
			OutputSchema:   outputSchema,
			RiskLevel:      tool.RiskLevel,
			ReadOnly:       tool.ReadOnly,
			TimeoutMS:      tool.TimeoutMS,
		})
	}

	return marshalCanonicalJSON(canonical)
}

// HashExecutionConfigV1 计算规范化 ExecutionConfigV1 的 SHA-256 小写十六进制摘要。
func HashExecutionConfigV1(input contracts.ExecutionConfigV1) (contracts.ExecutionConfigHash, error) {
	canonical, err := CanonicalExecutionConfigV1(input)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return contracts.ExecutionConfigHash(hex.EncodeToString(digest[:])), nil
}

type canonicalExecutionConfig struct {
	Schema        string                                  `json:"schema"`
	Version       uint32                                  `json:"version"`
	Agent         contracts.AgentExecutionConfigV1        `json:"agent"`
	Model         contracts.ModelExecutionConfigV1        `json:"model"`
	JSON          contracts.JSONExecutionContractV1       `json:"json"`
	Safety        contracts.SafetyExecutionContractV1     `json:"safety"`
	Planner       contracts.PlannerExecutionConfigV1      `json:"planner"`
	StepExecutor  contracts.StepExecutorExecutionConfigV1 `json:"step_executor"`
	ToolFramework canonicalToolFramework                  `json:"tool_framework"`
	Checkpoint    contracts.CheckpointExecutionConfigV1   `json:"checkpoint"`
	Approval      contracts.ApprovalExecutionConfigV1     `json:"approval"`
}

type canonicalToolFramework struct {
	ContractVersion       string                       `json:"contract_version"`
	ResultContractVersion string                       `json:"result_contract_version"`
	Tools                 []canonicalTool              `json:"tools"`
	AccessPolicy          contracts.ToolAccessPolicyV1 `json:"access_policy"`
	ResultLimits          contracts.ToolResultLimitsV1 `json:"result_limits"`
	EventPolicy           contracts.EventPolicyV1      `json:"event_policy"`
	PatchPolicy           contracts.PatchPolicyV1      `json:"patch_policy"`
}

type canonicalTool struct {
	Name           contracts.ToolName           `json:"name"`
	Enabled        bool                         `json:"enabled"`
	Description    string                       `json:"description"`
	CapabilityKind contracts.ToolCapabilityKind `json:"capability_kind"`
	InputSchema    json.RawMessage              `json:"input_schema"`
	OutputSchema   json.RawMessage              `json:"output_schema"`
	RiskLevel      contracts.RiskLevel          `json:"risk_level"`
	ReadOnly       bool                         `json:"read_only"`
	TimeoutMS      uint64                       `json:"timeout_ms"`
}

func validateSupportedExecutionConfigVersions(config contracts.ExecutionConfigV1) error {
	if config.Schema != contracts.ExecutionConfigSchemaV1 {
		return fmt.Errorf("normalize execution config: schema must be %q", contracts.ExecutionConfigSchemaV1)
	}
	if config.Version != contracts.ExecutionConfigVersionV1 {
		return fmt.Errorf("normalize execution config: version must be %d", contracts.ExecutionConfigVersionV1)
	}
	stringVersions := []struct {
		field string
		got   string
		want  string
	}{
		{"model.model_client_contract_version", config.Model.ModelClientContractVersion, supportedModelClientContractVersion},
		{"json.canonicalization_version", config.JSON.CanonicalizationVersion, supportedJSONCanonicalizationVersion},
		{"safety.sanitization_rule_version", config.Safety.SanitizationRuleVersion, supportedSanitizationRuleVersion},
		{"planner.contract_version", config.Planner.ContractVersion, supportedPlannerContractVersion},
		{"planner.non_tool_input_contract_version", config.Planner.NonToolInputContractVersion, supportedNonToolInputContractVersion},
		{"planner.tool_schema_subset_version", config.Planner.ToolSchemaSubsetVersion, supportedToolSchemaSubsetVersion},
		{"planner.repair_policy_version", config.Planner.RepairPolicyVersion, supportedRepairPolicyVersion},
		{"step_executor.contract_version", config.StepExecutor.ContractVersion, supportedStepExecutorContractVersion},
		{"step_executor.step_input_contract_version", config.StepExecutor.StepInputContractVersion, supportedStepInputContractVersion},
		{"step_executor.reference_protocol_version", config.StepExecutor.ReferenceProtocolVersion, supportedStepReferenceProtocolVersion},
		{"step_executor.reference_action_mode_version", config.StepExecutor.ReferenceActionModeVersion, supportedReferenceActionModeVersion},
		{"step_executor.output_schema_version", config.StepExecutor.OutputSchemaVersion, supportedOutputSchemaVersion},
		{"tool_framework.contract_version", config.ToolFramework.ContractVersion, supportedToolFrameworkContractVersion},
		{"tool_framework.result_contract_version", config.ToolFramework.ResultContractVersion, supportedToolFrameworkResultContractVersion},
		{"tool_framework.event_policy.version", config.ToolFramework.EventPolicy.Version, supportedEventPolicyVersion},
		{"tool_framework.patch_policy.version", config.ToolFramework.PatchPolicy.Version, supportedPatchPolicyVersion},
		{"tool_framework.patch_policy.response_classification_version", config.ToolFramework.PatchPolicy.ResponseClassificationVersion, supportedPatchResponseClassificationVersion},
		{"checkpoint.contract_version", config.Checkpoint.ContractVersion, supportedCheckpointContractVersion},
		{"checkpoint.resolved_reference_protocol_version", config.Checkpoint.ResolvedReferenceProtocolVersion, supportedCheckpointReferenceProtocolVersion},
		{"checkpoint.action_mode_version", config.Checkpoint.ActionModeVersion, supportedCheckpointActionModeVersion},
		{"approval.policy_version", config.Approval.PolicyVersion, supportedApprovalPolicyVersion},
	}
	for _, version := range stringVersions {
		if version.got != version.want {
			return fmt.Errorf("normalize execution config: %s must be %q", version.field, version.want)
		}
	}
	numericVersions := []struct {
		field string
		got   uint32
		want  uint32
	}{
		{"model.generation_params_schema_version", config.Model.GenerationParamsSchemaVersion, supportedGenerationParamsSchemaVersion},
		{"planner.plan_schema_version", config.Planner.PlanSchemaVersion, supportedPlanSchemaVersion},
		{"checkpoint.runtime_context_schema_version", config.Checkpoint.RuntimeContextSchemaVersion, supportedRuntimeContextSchemaVersion},
	}
	for _, version := range numericVersions {
		if version.got != version.want {
			return fmt.Errorf("normalize execution config: %s must be %d", version.field, version.want)
		}
	}
	return nil
}

func validateNormalizedExecutionConfig(config contracts.ExecutionConfigV1) error {
	requiredStrings := []struct {
		name  string
		value string
	}{
		{"agent.agent_id", string(config.Agent.AgentID)},
		{"agent.system_instruction", config.Agent.SystemInstruction},
		{"model.model_name", config.Model.ModelName},
		{"model.response_format", config.Model.ResponseFormat},
	}
	for _, field := range requiredStrings {
		if field.value == "" || !utf8.ValidString(field.value) {
			return fmt.Errorf("normalize execution config: %s must be non-empty valid UTF-8", field.name)
		}
	}
	if config.Agent.AllowedTools == nil || config.Planner.AllowedStepTypes == nil ||
		config.ToolFramework.Tools == nil || config.ToolFramework.AccessPolicy.Clusters == nil ||
		config.ToolFramework.AccessPolicy.ImageRegistryAllowlist == nil ||
		config.ToolFramework.EventPolicy.SortKeys == nil || config.ToolFramework.PatchPolicy.AllowedWriteFields == nil {
		return errors.New("normalize execution config: collection fields must not be null")
	}
	if config.Agent.MaxSteps == 0 {
		return errors.New("normalize execution config: agent.max_steps must be positive")
	}
	if config.Planner.SequenceStart == 0 {
		return errors.New("normalize execution config: planner.sequence_start must be positive")
	}
	if err := validateGenerationParams(config.Model.GenerationParams); err != nil {
		return err
	}
	for _, stepType := range config.Planner.AllowedStepTypes {
		if !stepType.Valid() {
			return fmt.Errorf("normalize execution config: invalid planner step type %q", stepType)
		}
	}
	if !config.Planner.FinalStepType.Valid() || !slices.Contains(config.Planner.AllowedStepTypes, config.Planner.FinalStepType) {
		return errors.New("normalize execution config: planner.final_step_type must be in allowed_step_types")
	}
	if err := validateTools(config); err != nil {
		return err
	}
	if err := validateAccessPolicy(config.ToolFramework.AccessPolicy); err != nil {
		return err
	}
	if err := validateEventPolicy(config.ToolFramework.EventPolicy); err != nil {
		return err
	}
	if !config.Approval.RequiredRiskLevel.Valid() {
		return errors.New("normalize execution config: approval.required_risk_level is invalid")
	}
	return validatePositiveLimits(config)
}

func validateGenerationParams(params contracts.GenerationParams) error {
	temperature, ok := new(big.Rat).SetString(params.Temperature.String())
	if !ok || temperature.Sign() < 0 || temperature.Cmp(big.NewRat(2, 1)) > 0 {
		return errors.New("normalize execution config: model.generation_params.temperature must be in [0,2]")
	}
	topP, ok := new(big.Rat).SetString(params.TopP.String())
	if !ok || topP.Sign() <= 0 || topP.Cmp(big.NewRat(1, 1)) > 0 {
		return errors.New("normalize execution config: model.generation_params.top_p must be in (0,1]")
	}
	if params.MaxOutputTokens == 0 || params.MaxOutputTokens > 8192 {
		return errors.New("normalize execution config: model.generation_params.max_output_tokens must be in [1,8192]")
	}
	return nil
}

func validateTools(config contracts.ExecutionConfigV1) error {
	known := make(map[contracts.ToolName]bool, len(config.ToolFramework.Tools))
	for _, tool := range config.ToolFramework.Tools {
		if tool.Name == "" || tool.Description == "" || !utf8.ValidString(tool.Description) {
			return errors.New("normalize execution config: tool name and description are required")
		}
		if !tool.CapabilityKind.Valid() || !tool.RiskLevel.Valid() || tool.TimeoutMS == 0 {
			return fmt.Errorf("normalize execution config: tool %q has invalid capability, risk, or timeout", tool.Name)
		}
		known[tool.Name] = true
	}
	for _, name := range config.Agent.AllowedTools {
		if name == "" || !known[name] {
			return fmt.Errorf("normalize execution config: allowed tool %q is not defined", name)
		}
	}
	return nil
}

func validateAccessPolicy(policy contracts.ToolAccessPolicyV1) error {
	for _, cluster := range policy.Clusters {
		if cluster.ClusterID == "" || !utf8.ValidString(cluster.ClusterID) || cluster.Namespaces == nil || cluster.Resources == nil {
			return errors.New("normalize execution config: cluster id and collections are required")
		}
		for _, namespace := range cluster.Namespaces {
			if namespace == "" || !utf8.ValidString(namespace) {
				return errors.New("normalize execution config: namespace must be non-empty valid UTF-8")
			}
		}
		for _, resource := range cluster.Resources {
			if resource.Kind == "" || resource.Verbs == nil || resource.WriteFields == nil {
				return errors.New("normalize execution config: resource kind and collections are required")
			}
			for _, verb := range resource.Verbs {
				if verb == "" {
					return errors.New("normalize execution config: resource verb must not be empty")
				}
			}
			for _, field := range resource.WriteFields {
				if field == "" {
					return errors.New("normalize execution config: resource write field must not be empty")
				}
			}
		}
	}
	if !policy.ReplicasPolicy.Enabled && (policy.ReplicasPolicy.Min != 0 || policy.ReplicasPolicy.Max != 0) {
		return errors.New("normalize execution config: disabled replicas policy must use min=0,max=0")
	}
	if policy.ReplicasPolicy.Enabled && (policy.ReplicasPolicy.Min < 0 || policy.ReplicasPolicy.Min > policy.ReplicasPolicy.Max) {
		return errors.New("normalize execution config: enabled replicas policy has invalid range")
	}
	return nil
}

func validateEventPolicy(policy contracts.EventPolicyV1) error {
	seen := make(map[contracts.EventSortKey]struct{}, len(policy.SortKeys))
	for _, key := range policy.SortKeys {
		if !key.Valid() {
			return fmt.Errorf("normalize execution config: invalid event sort key %q", key)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("normalize execution config: duplicate event sort key %q", key)
		}
		seen[key] = struct{}{}
	}
	if policy.CandidateBudgetBytes == 0 || policy.ReserveBytes == 0 || policy.ReserveBytes >= policy.CandidateBudgetBytes {
		return errors.New("normalize execution config: event byte budgets are invalid")
	}
	return nil
}

func validatePositiveLimits(config contracts.ExecutionConfigV1) error {
	planner := config.Planner.Limits
	plannerLimits := []uint64{
		planner.MaxTaskInputBytes, planner.MaxAgentPromptBytes, planner.MaxToolDescriptionBytes,
		planner.MaxToolSchemaBytes, planner.MaxPlanningTools, planner.MaxInitialPromptBytes,
		planner.MaxRepairPromptBytes, planner.MaxModelResponseBytes, planner.MaxPlanSteps,
		planner.MaxPlanDraftBytes, planner.MaxStepNameBytes, planner.MaxGoalBytes,
		planner.MaxStepInputBytes, planner.MaxResolvedReferencesPerStep, planner.MaxOutputFields,
		planner.MaxOutputFieldNameBytes, planner.MaxValidationIssues, planner.MaxRepairCandidateSummaryBytes,
		planner.PlannerModelCallTimeoutMS, planner.RepairMinModelBudgetMS, planner.PlannerLocalSafetyMarginMS,
	}
	step := config.StepExecutor.Limits
	stepLimits := []uint64{step.MaxResolvedStepInputBytes, step.MaxStepOutputBytes, step.MaxModelPromptBytes,
		step.MaxModelResponseBytes, step.MaxResolvedReferencesPerStep, step.MaxTargetPathDepth}
	tool := config.ToolFramework.ResultLimits
	toolLimits := []uint64{tool.RawResponseMaxBytes, tool.SafeDTOMaxBytes, uint64(tool.PodPageLimit),
		uint64(tool.EventPageLimit), uint64(tool.ContainerLogDefaultLines), uint64(tool.ContainerLogMaxLines)}
	for _, value := range append(append(plannerLimits, stepLimits...), toolLimits...) {
		if value == 0 {
			return errors.New("normalize execution config: resource limits must be positive")
		}
	}
	if config.JSON.MaxDepth == 0 || config.JSON.MaxObjectFields == 0 ||
		config.Safety.SafeSummaryMaxBytes == 0 || config.Safety.LogStringMaxBytes == 0 ||
		config.Checkpoint.MaxResolvedReferencesPerStep == 0 || config.Checkpoint.MaxTargetPathDepth == 0 {
		return errors.New("normalize execution config: JSON, safety, and checkpoint limits must be positive")
	}
	return nil
}

func normalizeJSONSchema(input contracts.CanonicalJSONSchema, field string) (contracts.CanonicalJSONSchema, error) {
	if !input.Type.Valid() {
		return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s type is invalid", field)
	}
	if input.Description != "" && !utf8.ValidString(input.Description) {
		return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s description is not valid UTF-8", field)
	}
	result := input
	switch input.Type {
	case contracts.JSONSchemaTypeObject:
		if input.Items != nil {
			return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s object must not define items", field)
		}
		if input.Properties == nil {
			result.Properties = map[string]contracts.CanonicalJSONSchema{}
		} else {
			result.Properties = make(map[string]contracts.CanonicalJSONSchema, len(input.Properties))
		}
		for name, child := range input.Properties {
			if name == "" || !utf8.ValidString(name) {
				return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s property name is invalid", field)
			}
			normalized, err := normalizeJSONSchema(child, field+".properties."+name)
			if err != nil {
				return contracts.CanonicalJSONSchema{}, err
			}
			result.Properties[name] = normalized
		}
		result.Required = sortedUnique(input.Required)
		if result.Required == nil {
			result.Required = []string{}
		}
		for _, name := range result.Required {
			if _, exists := result.Properties[name]; !exists {
				return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s required property %q is not defined", field, name)
			}
		}
		additionalProperties := false
		if input.AdditionalProperties != nil && *input.AdditionalProperties {
			return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s additionalProperties=true is not allowed", field)
		}
		result.AdditionalProperties = &additionalProperties
	case contracts.JSONSchemaTypeArray:
		if input.Items == nil || input.Properties != nil || input.Required != nil || input.AdditionalProperties != nil {
			return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s array must define only one items schema", field)
		}
		items, err := normalizeJSONSchema(*input.Items, field+".items")
		if err != nil {
			return contracts.CanonicalJSONSchema{}, err
		}
		result.Items = &items
	default:
		if input.Items != nil || input.Properties != nil || input.Required != nil || input.AdditionalProperties != nil {
			return contracts.CanonicalJSONSchema{}, fmt.Errorf("%s primitive has object or array fields", field)
		}
	}
	return result, nil
}

func marshalCanonicalSchema(schema contracts.CanonicalJSONSchema) (json.RawMessage, error) {
	value := make(map[string]any)
	if schema.Description != "" {
		value["description"] = schema.Description
	}
	if schema.Nullable {
		value["nullable"] = true
	}
	switch schema.Type {
	case contracts.JSONSchemaTypeObject:
		value["additionalProperties"] = false
		properties := make(map[string]json.RawMessage, len(schema.Properties))
		for name, child := range schema.Properties {
			encoded, err := marshalCanonicalSchema(child)
			if err != nil {
				return nil, err
			}
			properties[name] = encoded
		}
		value["properties"] = properties
		value["required"] = schema.Required
	case contracts.JSONSchemaTypeArray:
		encoded, err := marshalCanonicalSchema(*schema.Items)
		if err != nil {
			return nil, err
		}
		value["items"] = encoded
	}
	value["type"] = schema.Type
	encoded, err := marshalCanonicalJSON(value)
	return json.RawMessage(encoded), err
}

func marshalCanonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode canonical JSON: %w", err)
	}
	result := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	result = bytes.ReplaceAll(result, []byte(`\u2028`), []byte("\u2028"))
	result = bytes.ReplaceAll(result, []byte(`\u2029`), []byte("\u2029"))
	return result, nil
}

func sortedUnique[S ~[]E, E ~string](input S) S {
	if input == nil {
		return nil
	}
	output := slices.Clone(input)
	slices.Sort(output)
	return slices.Compact(output)
}

func compactTools(tools []contracts.ToolDefinitionV1) ([]contracts.ToolDefinitionV1, error) {
	return compactNamed(tools, func(tool contracts.ToolDefinitionV1) string { return string(tool.Name) }, "tool")
}

func compactClusters(clusters []contracts.ClusterPolicyV1) ([]contracts.ClusterPolicyV1, error) {
	return compactNamed(clusters, func(cluster contracts.ClusterPolicyV1) string { return cluster.ClusterID }, "cluster")
}

func compactResources(resources []contracts.ResourcePolicyV1) ([]contracts.ResourcePolicyV1, error) {
	return compactNamed(resources, func(resource contracts.ResourcePolicyV1) string { return resource.Kind }, "resource")
}

func compactNamed[S ~[]E, E any](values S, name func(E) string, kind string) (S, error) {
	if len(values) < 2 {
		return values, nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		last := result[len(result)-1]
		if name(last) != name(value) {
			result = append(result, value)
			continue
		}
		if !reflect.DeepEqual(last, value) {
			return nil, fmt.Errorf("normalize execution config: conflicting %s %q", kind, name(value))
		}
	}
	return result, nil
}

package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// PlanningToolSnapshotSchemaVersionV1 是当前唯一支持的 Catalog 快照版本。
	PlanningToolSnapshotSchemaVersionV1 uint32 = 1

	maxPlanningToolCount       = 32
	maxPlanningToolDescription = 4 * 1024
	maxPlanningToolSchemaBytes = 64 * 1024
	maxPlanningToolSchemaDepth = 16
	maxPlanningSchemaFields    = 64
)

// ValidatePlanningToolCatalogSelector 校验唯一共享 Catalog selector 契约。
func ValidatePlanningToolCatalogSelector(selector PlanningToolCatalogSelector) error {
	if !validCatalogString(selector.CatalogID) || !validCatalogString(selector.ExpectedRegistryVersion) ||
		!selector.ExpectedSnapshotHash.Valid() || selector.AllowedTools == nil ||
		len(selector.AllowedTools) > maxPlanningToolCount {
		return NewPlanningToolCatalogError(
			PlanningToolCatalogErrorToolConfigInvalid,
			nil,
			CauseCodeRuntimeStaticToolSnapshotInconsistent,
			nil,
		)
	}
	seen := make(map[string]struct{}, len(selector.AllowedTools))
	for _, toolName := range selector.AllowedTools {
		if !validCatalogString(toolName) {
			return NewPlanningToolCatalogError(
				PlanningToolCatalogErrorToolConfigInvalid,
				nil,
				CauseCodeRuntimeStaticToolSnapshotInconsistent,
				nil,
			)
		}
		if _, exists := seen[toolName]; exists {
			name := toolName
			return NewPlanningToolCatalogError(
				PlanningToolCatalogErrorDuplicateTool,
				&name,
				CauseCodeRuntimeStaticToolSnapshotInconsistent,
				nil,
			)
		}
		seen[toolName] = struct{}{}
	}
	return nil
}

// CanonicalPlanningToolSnapshotPayload 返回 Catalog hash 使用的 RFC 8785 JCS bytes。
// snapshot_hash 本身不参与 payload；tools 总是按 tool_name Unicode code point 排序。
func CanonicalPlanningToolSnapshotPayload(
	catalogID string,
	snapshot PlanningToolSnapshot,
) ([]byte, error) {
	if !validCatalogString(catalogID) {
		return nil, errors.New("canonical planning tool snapshot: catalog_id is invalid")
	}
	tools := clonePlanningToolSpecs(snapshot.Tools)
	slices.SortFunc(tools, func(left, right PlanningToolSpec) int {
		return strings.Compare(left.ToolName, right.ToolName)
	})
	toolValues := make([]any, len(tools))
	for index, tool := range tools {
		schema, err := planningToolSchemaValue(tool.InputSchema, 1)
		if err != nil {
			return nil, fmt.Errorf("canonical planning tool snapshot: input schema: %w", err)
		}
		toolValues[index] = map[string]any{
			"capability": map[string]any{
				"kind":       string(tool.Capability.Kind),
				"read_only":  tool.Capability.ReadOnly,
				"risk_level": string(tool.Capability.RiskLevel),
			},
			"description":  tool.Description,
			"enabled":      tool.Enabled,
			"input_schema": schema,
			"tool_name":    tool.ToolName,
		}
	}
	payload := map[string]any{
		"catalog_id":       catalogID,
		"registry_version": snapshot.RegistryVersion,
		"schema_version":   snapshot.SchemaVersion,
		"tools":            toolValues,
	}
	var encoded bytes.Buffer
	if err := writeJCSValue(&encoded, payload); err != nil {
		return nil, fmt.Errorf("canonical planning tool snapshot: %w", err)
	}
	return encoded.Bytes(), nil
}

// ComputePlanningToolSnapshotHash 计算 Catalog 专用 hash，并返回参与计算的 JCS bytes。
func ComputePlanningToolSnapshotHash(
	catalogID string,
	snapshot PlanningToolSnapshot,
) (CatalogSnapshotHash, []byte, error) {
	payload, err := CanonicalPlanningToolSnapshotPayload(catalogID, snapshot)
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(payload)
	return CatalogSnapshotHash(hex.EncodeToString(digest[:])), payload, nil
}

// ValidatePlanningToolSnapshot 验证 Planner 收到的完整 DTO、selector 证据和 JCS hash。
// 返回值仅使用共享类型化 Catalog 错误；响应结构损坏归类为 RuntimeFatal。
func ValidatePlanningToolSnapshot(
	selector PlanningToolCatalogSelector,
	snapshot PlanningToolSnapshot,
) error {
	if err := ValidatePlanningToolCatalogSelector(selector); err != nil {
		return err
	}
	if snapshot.SchemaVersion != PlanningToolSnapshotSchemaVersionV1 ||
		!validCatalogString(snapshot.RegistryVersion) || !snapshot.SnapshotHash.Valid() ||
		snapshot.Tools == nil || len(snapshot.Tools) > maxPlanningToolCount ||
		len(snapshot.Tools) != len(selector.AllowedTools) {
		return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, nil)
	}

	allowed := make(map[string]struct{}, len(selector.AllowedTools))
	for _, name := range selector.AllowedTools {
		allowed[name] = struct{}{}
	}
	capabilities := make(map[ToolCapabilityKind]struct{}, len(snapshot.Tools))
	previousName := ""
	for index, tool := range snapshot.Tools {
		if !validCatalogString(tool.ToolName) || !validCatalogString(tool.Description) ||
			len(tool.Description) > maxPlanningToolDescription || !tool.Enabled ||
			!validPlanningToolCapability(tool.Capability) {
			return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, nil)
		}
		if index > 0 && strings.Compare(previousName, tool.ToolName) >= 0 {
			return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, nil)
		}
		previousName = tool.ToolName
		if _, exists := allowed[tool.ToolName]; !exists {
			return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, nil)
		}
		delete(allowed, tool.ToolName)
		if _, exists := capabilities[tool.Capability.Kind]; exists {
			return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, nil)
		}
		capabilities[tool.Capability.Kind] = struct{}{}
		schemaValue, err := planningToolSchemaValue(tool.InputSchema, 1)
		if err != nil || tool.InputSchema.Type != JSONSchemaTypeObject || tool.InputSchema.Nullable {
			return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, err)
		}
		var schemaBytes bytes.Buffer
		if err := writeJCSValue(&schemaBytes, schemaValue); err != nil || schemaBytes.Len() > maxPlanningToolSchemaBytes {
			return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, err)
		}
	}
	if len(allowed) != 0 {
		return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, nil)
	}

	computed, _, err := ComputePlanningToolSnapshotHash(selector.CatalogID, snapshot)
	if err != nil {
		return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, err)
	}
	if computed != snapshot.SnapshotHash {
		return catalogSnapshotError(PlanningToolCatalogErrorRuntimeFatal, nil)
	}
	if snapshot.RegistryVersion != selector.ExpectedRegistryVersion ||
		snapshot.SnapshotHash != selector.ExpectedSnapshotHash {
		return catalogSnapshotError(PlanningToolCatalogErrorConfigVersionMismatch, nil)
	}
	return nil
}

func catalogSnapshotError(kind PlanningToolCatalogErrorKind, cause error) error {
	return NewPlanningToolCatalogError(
		kind,
		nil,
		CauseCodeRuntimeStaticToolSnapshotInconsistent,
		cause,
	)
}

func validCatalogString(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != ""
}

func validPlanningToolCapability(capability PlanningToolCapability) bool {
	if !capability.Kind.Valid() || !capability.RiskLevel.Valid() {
		return false
	}
	if capability.Kind == ToolCapabilityK8sPatchDeployment {
		return capability.RiskLevel == RiskLevelHigh && !capability.ReadOnly
	}
	return capability.RiskLevel == RiskLevelLow && capability.ReadOnly
}

func planningToolSchemaValue(schema CanonicalJSONSchema, depth int) (map[string]any, error) {
	if depth > maxPlanningToolSchemaDepth || !schema.Type.Valid() ||
		(schema.Description != "" && !utf8.ValidString(schema.Description)) {
		return nil, errors.New("schema is not normalized")
	}
	value := map[string]any{"type": string(schema.Type)}
	if schema.Description != "" {
		value["description"] = schema.Description
	}
	if schema.Nullable {
		value["nullable"] = true
	}
	switch schema.Type {
	case JSONSchemaTypeObject:
		if schema.Items != nil || schema.Properties == nil || schema.Required == nil ||
			schema.AdditionalProperties == nil || *schema.AdditionalProperties ||
			len(schema.Properties) > maxPlanningSchemaFields {
			return nil, errors.New("object schema is not normalized")
		}
		properties := make(map[string]any, len(schema.Properties))
		for name, child := range schema.Properties {
			if !validCatalogString(name) {
				return nil, errors.New("schema property name is invalid")
			}
			childValue, err := planningToolSchemaValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			properties[name] = childValue
		}
		if !slices.IsSorted(schema.Required) {
			return nil, errors.New("required properties are not sorted")
		}
		seen := make(map[string]struct{}, len(schema.Required))
		for _, required := range schema.Required {
			if !validCatalogString(required) {
				return nil, errors.New("required property is invalid")
			}
			if _, exists := schema.Properties[required]; !exists {
				return nil, errors.New("required property is not defined")
			}
			if _, exists := seen[required]; exists {
				return nil, errors.New("required property is duplicated")
			}
			seen[required] = struct{}{}
		}
		required := make([]any, len(schema.Required))
		for index, name := range schema.Required {
			required[index] = name
		}
		value["additionalProperties"] = false
		value["properties"] = properties
		value["required"] = required
	case JSONSchemaTypeArray:
		if schema.Items == nil || schema.Properties != nil || schema.Required != nil ||
			schema.AdditionalProperties != nil {
			return nil, errors.New("array schema is not normalized")
		}
		items, err := planningToolSchemaValue(*schema.Items, depth+1)
		if err != nil {
			return nil, err
		}
		value["items"] = items
	default:
		if schema.Items != nil || schema.Properties != nil || schema.Required != nil ||
			schema.AdditionalProperties != nil {
			return nil, errors.New("primitive schema is not normalized")
		}
	}
	return value, nil
}

func clonePlanningToolSpecs(input []PlanningToolSpec) []PlanningToolSpec {
	if input == nil {
		return nil
	}
	result := make([]PlanningToolSpec, len(input))
	for index := range input {
		result[index] = input[index]
		result[index].InputSchema = cloneCatalogSchema(input[index].InputSchema)
	}
	return result
}

func cloneCatalogSchema(input CanonicalJSONSchema) CanonicalJSONSchema {
	result := input
	if input.Items != nil {
		items := cloneCatalogSchema(*input.Items)
		result.Items = &items
	}
	if input.Properties != nil {
		result.Properties = make(map[string]CanonicalJSONSchema, len(input.Properties))
		for name, child := range input.Properties {
			result.Properties[name] = cloneCatalogSchema(child)
		}
	}
	result.Required = slices.Clone(input.Required)
	if input.AdditionalProperties != nil {
		additional := *input.AdditionalProperties
		result.AdditionalProperties = &additional
	}
	return result
}

func writeJCSValue(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case string:
		return writeJCSString(output, typed)
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
		return nil
	case uint32:
		fmt.Fprintf(output, "%d", typed)
		return nil
	case []any:
		output.WriteByte('[')
		for index, element := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeJCSValue(output, element); err != nil {
				return err
			}
		}
		output.WriteByte(']')
		return nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return errors.New("object member name is not valid UTF-8")
			}
			keys = append(keys, key)
		}
		sort.Slice(keys, func(left, right int) bool {
			return compareUTF16(keys[left], keys[right]) < 0
		})
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeJCSString(output, key); err != nil {
				return err
			}
			output.WriteByte(':')
			if err := writeJCSValue(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
		return nil
	default:
		return fmt.Errorf("unsupported JCS value type %T", value)
	}
}

func writeJCSString(output *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return errors.New("string is not valid UTF-8")
	}
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(character)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if character <= 0x1f {
				fmt.Fprintf(output, `\u%04x`, character)
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
	return nil
}

func compareUTF16(left, right string) int {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	limit := min(len(leftUnits), len(rightUnits))
	for index := 0; index < limit; index++ {
		if leftUnits[index] < rightUnits[index] {
			return -1
		}
		if leftUnits[index] > rightUnits[index] {
			return 1
		}
	}
	return len(leftUnits) - len(rightUnits)
}

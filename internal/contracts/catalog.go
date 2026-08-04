package contracts

import (
	"context"
	"fmt"
)

// PlanningToolCatalogPort 加载启动时冻结的 Planning Tool 投影。
type PlanningToolCatalogPort interface {
	LoadPlanningToolSnapshot(
		ctx context.Context,
		selector PlanningToolCatalogSelector,
	) (PlanningToolSnapshot, error)
}

// PlanningToolCatalogSelector 表示 Task Runtime 冻结的 Catalog 选择证据。
type PlanningToolCatalogSelector struct {
	CatalogID               string              `json:"catalog_id"`
	AllowedTools            []string            `json:"allowed_tools"`
	ExpectedRegistryVersion string              `json:"expected_registry_version"`
	ExpectedSnapshotHash    CatalogSnapshotHash `json:"expected_snapshot_hash"`
}

// PlanningToolSnapshot 表示 Tool Framework 返回的完整规划投影。
type PlanningToolSnapshot struct {
	SchemaVersion   uint32              `json:"schema_version"`
	RegistryVersion string              `json:"registry_version"`
	SnapshotHash    CatalogSnapshotHash `json:"snapshot_hash"`
	Tools           []PlanningToolSpec  `json:"tools"`
}

// PlanningToolSpec 表示单个模型可见 Tool 规范。
type PlanningToolSpec struct {
	ToolName    string                 `json:"tool_name"`
	Description string                 `json:"description"`
	InputSchema CanonicalJSONSchema    `json:"input_schema"`
	Capability  PlanningToolCapability `json:"capability"`
	Enabled     bool                   `json:"enabled"`
}

// PlanningToolCapability 表示 Planning Tool 的执行能力投影。
type PlanningToolCapability struct {
	Kind      ToolCapabilityKind `json:"kind"`
	RiskLevel RiskLevel          `json:"risk_level"`
	ReadOnly  bool               `json:"read_only"`
}

// PlanningToolCatalogErrorKind 表示 Catalog Port 的封闭错误类别。
type PlanningToolCatalogErrorKind string

const (
	PlanningToolCatalogErrorToolNotFound          PlanningToolCatalogErrorKind = "ToolNotFound"
	PlanningToolCatalogErrorToolDisabled          PlanningToolCatalogErrorKind = "ToolDisabled"
	PlanningToolCatalogErrorDuplicateTool         PlanningToolCatalogErrorKind = "DuplicateTool"
	PlanningToolCatalogErrorToolConfigInvalid     PlanningToolCatalogErrorKind = "ToolConfigInvalid"
	PlanningToolCatalogErrorConfigVersionMismatch PlanningToolCatalogErrorKind = "ConfigVersionMismatch"
	PlanningToolCatalogErrorRuntimeFatal          PlanningToolCatalogErrorKind = "RuntimeFatal"
)

// Valid 报告 PlanningToolCatalogErrorKind 是否属于封闭集合。
func (k PlanningToolCatalogErrorKind) Valid() bool {
	switch k {
	case PlanningToolCatalogErrorToolNotFound, PlanningToolCatalogErrorToolDisabled,
		PlanningToolCatalogErrorDuplicateTool, PlanningToolCatalogErrorToolConfigInvalid,
		PlanningToolCatalogErrorConfigVersionMismatch, PlanningToolCatalogErrorRuntimeFatal:
		return true
	default:
		return false
	}
}

// PlanningToolCatalogError 是可通过 errors.As 识别的 Catalog 错误。
type PlanningToolCatalogError struct {
	Kind      PlanningToolCatalogErrorKind
	ToolName  *string
	CauseCode CauseCode
	cause     error
}

// NewPlanningToolCatalogError 创建类型化 Catalog 错误。
func NewPlanningToolCatalogError(
	kind PlanningToolCatalogErrorKind,
	toolName *string,
	causeCode CauseCode,
	cause error,
) *PlanningToolCatalogError {
	return &PlanningToolCatalogError{
		Kind:      kind,
		ToolName:  toolName,
		CauseCode: causeCode,
		cause:     cause,
	}
}

// Error 返回不包含底层原始错误文本的稳定描述。
func (e *PlanningToolCatalogError) Error() string {
	if e == nil {
		return "planning tool catalog error"
	}
	return fmt.Sprintf("planning tool catalog error: %s", e.Kind)
}

// Unwrap 保留 context 取消和 deadline 的 errors.Is 语义。
func (e *PlanningToolCatalogError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

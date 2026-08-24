package contracts

import (
	"bytes"
	"errors"
	"testing"
)

func TestCatalogSnapshotJCSFixture(t *testing.T) {
	t.Parallel()

	snapshot := PlanningToolSnapshot{
		SchemaVersion:   PlanningToolSnapshotSchemaVersionV1,
		RegistryVersion: "registry-v1",
		Tools:           []PlanningToolSpec{},
	}
	hash, payload, err := ComputePlanningToolSnapshotHash("catalog-a", snapshot)
	if err != nil {
		t.Fatalf("ComputePlanningToolSnapshotHash() error = %v", err)
	}
	wantPayload := []byte(`{"catalog_id":"catalog-a","registry_version":"registry-v1","schema_version":1,"tools":[]}`)
	if !bytes.Equal(payload, wantPayload) {
		t.Fatalf("payload = %s, want %s", payload, wantPayload)
	}
	const wantHash CatalogSnapshotHash = "12bbdd252a92c99246873340c39da2d69ab001972bde1a8e90c3bf3d3af78cdf"
	if hash != wantHash {
		t.Fatalf("hash = %s, want %s", hash, wantHash)
	}
}

func TestCatalogSnapshotJCSSortsToolsAndPreservesNormalizedSchema(t *testing.T) {
	t.Parallel()

	snapshot := PlanningToolSnapshot{
		SchemaVersion:   PlanningToolSnapshotSchemaVersionV1,
		RegistryVersion: "registry-v1",
		Tools: []PlanningToolSpec{
			catalogSnapshotTool("tool.z", ToolCapabilityK8sGetPod),
			catalogSnapshotTool("tool.a", ToolCapabilityK8sGetDeployment),
		},
	}
	_, first, err := ComputePlanningToolSnapshotHash("catalog-a", snapshot)
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}
	snapshot.Tools[0], snapshot.Tools[1] = snapshot.Tools[1], snapshot.Tools[0]
	_, second, err := ComputePlanningToolSnapshotHash("catalog-a", snapshot)
	if err != nil {
		t.Fatalf("second hash: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("tool input order changed JCS payload:\n%s\n%s", first, second)
	}
	if !bytes.Contains(first, []byte(`"additionalProperties":false`)) ||
		!bytes.Contains(first, []byte(`"properties":{}`)) ||
		!bytes.Contains(first, []byte(`"required":[]`)) {
		t.Fatalf("normalized empty object schema fields missing: %s", first)
	}
}

func TestValidatePlanningToolCatalogSelector(t *testing.T) {
	t.Parallel()

	valid := PlanningToolCatalogSelector{
		CatalogID:               "catalog-a",
		AllowedTools:            []string{},
		ExpectedRegistryVersion: "registry-v1",
		ExpectedSnapshotHash:    CatalogSnapshotHash("12bbdd252a92c99246873340c39da2d69ab001972bde1a8e90c3bf3d3af78cdf"),
	}
	if err := ValidatePlanningToolCatalogSelector(valid); err != nil {
		t.Fatalf("valid selector rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*PlanningToolCatalogSelector)
		kind PlanningToolCatalogErrorKind
	}{
		{name: "nil allowed", edit: func(value *PlanningToolCatalogSelector) { value.AllowedTools = nil }, kind: PlanningToolCatalogErrorToolConfigInvalid},
		{name: "blank catalog", edit: func(value *PlanningToolCatalogSelector) { value.CatalogID = " " }, kind: PlanningToolCatalogErrorToolConfigInvalid},
		{name: "bad registry", edit: func(value *PlanningToolCatalogSelector) { value.ExpectedRegistryVersion = "" }, kind: PlanningToolCatalogErrorToolConfigInvalid},
		{name: "bad hash", edit: func(value *PlanningToolCatalogSelector) { value.ExpectedSnapshotHash = "bad" }, kind: PlanningToolCatalogErrorToolConfigInvalid},
		{name: "empty tool", edit: func(value *PlanningToolCatalogSelector) { value.AllowedTools = []string{""} }, kind: PlanningToolCatalogErrorToolConfigInvalid},
		{name: "duplicate tool", edit: func(value *PlanningToolCatalogSelector) { value.AllowedTools = []string{"tool.a", "tool.a"} }, kind: PlanningToolCatalogErrorDuplicateTool},
		{name: "too many tools", edit: func(value *PlanningToolCatalogSelector) {
			value.AllowedTools = make([]string, maxPlanningToolCount+1)
			for index := range value.AllowedTools {
				value.AllowedTools[index] = string(rune('a' + index))
			}
		}, kind: PlanningToolCatalogErrorToolConfigInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := valid
			selector.AllowedTools = append([]string(nil), valid.AllowedTools...)
			test.edit(&selector)
			err := ValidatePlanningToolCatalogSelector(selector)
			assertCatalogErrorKind(t, err, test.kind)
		})
	}
}

func TestValidatePlanningToolSnapshotRejectsTampering(t *testing.T) {
	t.Parallel()

	selector, snapshot := catalogSnapshotFixture(t, "catalog-a", []PlanningToolSpec{
		catalogSnapshotTool("tool.a", ToolCapabilityK8sGetDeployment),
		catalogSnapshotTool("tool.z", ToolCapabilityK8sGetPod),
	})
	if err := ValidatePlanningToolSnapshot(selector, snapshot); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*PlanningToolCatalogSelector, *PlanningToolSnapshot)
		kind PlanningToolCatalogErrorKind
	}{
		{name: "registry mismatch", edit: func(selector *PlanningToolCatalogSelector, _ *PlanningToolSnapshot) {
			selector.ExpectedRegistryVersion = "registry-v2"
		}, kind: PlanningToolCatalogErrorConfigVersionMismatch},
		{name: "hash mismatch", edit: func(selector *PlanningToolCatalogSelector, _ *PlanningToolSnapshot) {
			selector.ExpectedSnapshotHash = CatalogSnapshotHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		}, kind: PlanningToolCatalogErrorConfigVersionMismatch},
		{name: "unsupported schema version", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.SchemaVersion = 2 }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "invalid registry version", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.RegistryVersion = "" }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "invalid snapshot hash", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.SnapshotHash = "bad" }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "nil tools", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.Tools = nil }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "unsorted tools", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) {
			value.Tools[0], value.Tools[1] = value.Tools[1], value.Tools[0]
		}, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "missing tool", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.Tools = value.Tools[:1] }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "unrequested tool", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.Tools[1].ToolName = "tool.x" }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "disabled tool", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.Tools[0].Enabled = false }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "blank description", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) { value.Tools[0].Description = "" }, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "invalid capability", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) {
			value.Tools[0].Capability.ReadOnly = false
		}, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "duplicate capability", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) {
			value.Tools[1].Capability = value.Tools[0].Capability
		}, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "non canonical schema", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) {
			value.Tools[0].InputSchema.Required = nil
		}, kind: PlanningToolCatalogErrorRuntimeFatal},
		{name: "modified payload with unchanged hash", edit: func(_ *PlanningToolCatalogSelector, value *PlanningToolSnapshot) {
			value.Tools[0].Description = "changed"
		}, kind: PlanningToolCatalogErrorRuntimeFatal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotSelector := selector
			gotSelector.AllowedTools = append([]string(nil), selector.AllowedTools...)
			gotSnapshot := cloneCatalogSnapshotForTest(snapshot)
			test.edit(&gotSelector, &gotSnapshot)
			err := ValidatePlanningToolSnapshot(gotSelector, gotSnapshot)
			assertCatalogErrorKind(t, err, test.kind)
		})
	}
}

func catalogSnapshotFixture(
	t *testing.T,
	catalogID string,
	tools []PlanningToolSpec,
) (PlanningToolCatalogSelector, PlanningToolSnapshot) {
	t.Helper()
	snapshot := PlanningToolSnapshot{
		SchemaVersion:   PlanningToolSnapshotSchemaVersionV1,
		RegistryVersion: "registry-v1",
		Tools:           tools,
	}
	hash, _, err := ComputePlanningToolSnapshotHash(catalogID, snapshot)
	if err != nil {
		t.Fatalf("compute fixture hash: %v", err)
	}
	snapshot.SnapshotHash = hash
	allowed := make([]string, len(tools))
	for index := range tools {
		allowed[index] = tools[index].ToolName
	}
	return PlanningToolCatalogSelector{
		CatalogID:               catalogID,
		AllowedTools:            allowed,
		ExpectedRegistryVersion: snapshot.RegistryVersion,
		ExpectedSnapshotHash:    hash,
	}, snapshot
}

func catalogSnapshotTool(name string, kind ToolCapabilityKind) PlanningToolSpec {
	additional := false
	capability := PlanningToolCapability{Kind: kind, RiskLevel: RiskLevelLow, ReadOnly: true}
	if kind == ToolCapabilityK8sPatchDeployment {
		capability.RiskLevel = RiskLevelHigh
		capability.ReadOnly = false
	}
	return PlanningToolSpec{
		ToolName:    name,
		Description: "Safe planning description.",
		InputSchema: CanonicalJSONSchema{
			Type:                 JSONSchemaTypeObject,
			Properties:           map[string]CanonicalJSONSchema{},
			Required:             []string{},
			AdditionalProperties: &additional,
		},
		Capability: capability,
		Enabled:    true,
	}
}

func cloneCatalogSnapshotForTest(input PlanningToolSnapshot) PlanningToolSnapshot {
	result := input
	result.Tools = clonePlanningToolSpecs(input.Tools)
	return result
}

func assertCatalogErrorKind(t *testing.T, err error, want PlanningToolCatalogErrorKind) {
	t.Helper()
	var typed *PlanningToolCatalogError
	if !errors.As(err, &typed) {
		t.Fatalf("error %v is not PlanningToolCatalogError", err)
	}
	if typed.Kind != want {
		t.Fatalf("error kind = %s, want %s", typed.Kind, want)
	}
	if typed.CauseCode != CauseCodeRuntimeStaticToolSnapshotInconsistent {
		t.Fatalf("cause code = %s", typed.CauseCode)
	}
}

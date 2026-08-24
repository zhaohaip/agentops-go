// Package catalogcontract 提供 Planning Tool Catalog Port 的共享契约测试。
package catalogcontract

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// Factory 从同一场景创建被测 Provider。
// Planner Fake 消费 Responses；后续 Static Registry Adapter 使用 Fixture 构造真实实现。
type Factory interface {
	New(testing.TB, Scenario) contracts.PlanningToolCatalogPort
}

// FactoryFunc 允许用函数装配 Catalog Provider factory。
type FactoryFunc func(testing.TB, Scenario) contracts.PlanningToolCatalogPort

// New 实现 Factory。
func (function FactoryFunc) New(t testing.TB, scenario Scenario) contracts.PlanningToolCatalogPort {
	return function(t, scenario)
}

// PlannerValidator 验证共享 Snapshot 能被 Planner 的真实 Validator 直接消费。
type PlannerValidator func(testing.TB, contracts.PlanningToolCatalogSelector, contracts.PlanningToolSnapshot)

// Fixture 描述契约测试所需的不可变 Catalog 集合。
type Fixture struct {
	Catalogs []Catalog
}

// Catalog 描述一个静态 Registry 的测试事实。
type Catalog struct {
	CatalogID       string
	RegistryVersion string
	Tools           []contracts.PlanningToolSpec
	Unreadable      bool
}

// Scenario 同时提供真实 Adapter 所需的 Registry fixture 和 Fake 所需的 FIFO 响应。
type Scenario struct {
	Fixture   Fixture
	Responses []Response
}

// Response 表示严格 Fake 的一次共享 DTO/error 响应。
type Response struct {
	Snapshot contracts.PlanningToolSnapshot
	Err      error
}

const (
	// FixedCatalogID 是 PL-TC-010 跨实现固定 fixture 的 Catalog ID。
	FixedCatalogID = "catalog-fixture"
	// FixedRegistryVersion 是 PL-TC-010 跨实现固定 fixture 的 Registry version。
	FixedRegistryVersion = "registry-v1"
	// FixedCanonicalPayload 是 PL-TC-010 固定 RFC 8785 JCS bytes。
	FixedCanonicalPayload = `{"catalog_id":"catalog-fixture","registry_version":"registry-v1","schema_version":1,"tools":[{"capability":{"kind":"K8S_GET_DEPLOYMENT","read_only":true,"risk_level":"Low"},"description":"Get one Deployment.","enabled":true,"input_schema":{"additionalProperties":false,"properties":{"cluster":{"type":"string"},"deployment":{"type":"string"},"namespace":{"type":"string"}},"required":["cluster","deployment","namespace"],"type":"object"},"tool_name":"k8s.get_deployment"},{"capability":{"kind":"K8S_GET_POD","read_only":true,"risk_level":"Low"},"description":"Get one Pod.","enabled":true,"input_schema":{"additionalProperties":false,"properties":{"cluster":{"type":"string"},"namespace":{"type":"string"},"pod":{"type":"string"}},"required":["cluster","namespace","pod"],"type":"object"},"tool_name":"k8s.get_pod"}]}`
	// FixedSnapshotHash 是 FixedCanonicalPayload 的固定 SHA-256 lower-hex。
	FixedSnapshotHash contracts.CatalogSnapshotHash = "10233fc6c4d36ab20e065b63a5bbd915df913745de6130e56b96dedf250d6ddf"
)

// Run 对 factory 创建的 Provider 执行完整 PL-TC-001～012 契约套件。
func Run(t *testing.T, factory Factory, validate PlannerValidator) {
	t.Helper()
	if factory == nil || validate == nil {
		t.Fatal("Catalog contract factory and Planner validator are required")
	}
	t.Run("PL-TC-001 order independent selector", func(t *testing.T) {
		fixture := standardFixture()
		selector := SelectorFor(t, fixture, "catalog-a", []string{"tool.read", "tool.pod"})
		expected := SnapshotFor(t, fixture, "catalog-a", selector.AllowedTools)
		provider := newProvider(t, factory, fixture, success(expected), success(expected))
		first := loadSuccess(t, provider, selector)
		reversed := cloneSelector(selector)
		slices.Reverse(reversed.AllowedTools)
		second := loadSuccess(t, provider, reversed)
		if !reflect.DeepEqual(first, second) || first.Tools[0].ToolName != "tool.pod" ||
			first.Tools[1].ToolName != "tool.read" {
			t.Fatalf("selector order changed sorted snapshot: %+v %+v", first, second)
		}
	})
	t.Run("PL-TC-002 empty and nil allowed tools", func(t *testing.T) {
		fixture := standardFixture()
		selector := SelectorFor(t, fixture, "catalog-a", []string{})
		provider := newProvider(t, factory, fixture,
			success(SnapshotFor(t, fixture, "catalog-a", selector.AllowedTools)),
			failure(contracts.PlanningToolCatalogErrorToolConfigInvalid, ""),
		)
		snapshot := loadSuccess(t, provider, selector)
		if snapshot.Tools == nil || len(snapshot.Tools) != 0 {
			t.Fatalf("empty projection tools = %#v", snapshot.Tools)
		}
		selector.AllowedTools = nil
		loadError(t, provider, selector, contracts.PlanningToolCatalogErrorToolConfigInvalid)
	})
	t.Run("PL-TC-003 missing tool", func(t *testing.T) {
		fixture := standardFixture()
		selector := SelectorFor(t, fixture, "catalog-a", []string{})
		selector.AllowedTools = []string{"tool.missing"}
		loadError(t, newProvider(t, factory, fixture,
			failure(contracts.PlanningToolCatalogErrorToolNotFound, "tool.missing")),
			selector, contracts.PlanningToolCatalogErrorToolNotFound)
	})
	t.Run("PL-TC-004 disabled tool", func(t *testing.T) {
		fixture := standardFixture()
		selector := SelectorFor(t, fixture, "catalog-a", []string{})
		selector.AllowedTools = []string{"tool.disabled"}
		loadError(t, newProvider(t, factory, fixture,
			failure(contracts.PlanningToolCatalogErrorToolDisabled, "tool.disabled")),
			selector, contracts.PlanningToolCatalogErrorToolDisabled)
	})
	t.Run("PL-TC-005 duplicate tool", func(t *testing.T) {
		fixture := standardFixture()
		selector := SelectorFor(t, fixture, "catalog-a", []string{})
		selector.AllowedTools = []string{"tool.read", "tool.read"}
		loadError(t, newProvider(t, factory, fixture,
			failure(contracts.PlanningToolCatalogErrorDuplicateTool, "tool.read")),
			selector, contracts.PlanningToolCatalogErrorDuplicateTool)
	})
	t.Run("PL-TC-006 invalid registry facts", func(t *testing.T) {
		tests := []struct {
			name string
			edit func(*Catalog)
		}{
			{name: "description", edit: func(catalog *Catalog) { catalog.Tools[0].Description = "" }},
			{name: "schema", edit: func(catalog *Catalog) { catalog.Tools[0].InputSchema.Required = nil }},
			{name: "capability", edit: func(catalog *Catalog) { catalog.Tools[0].Capability.ReadOnly = false }},
			{name: "version", edit: func(catalog *Catalog) { catalog.RegistryVersion = "" }},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				fixture := standardFixture()
				test.edit(&fixture.Catalogs[0])
				selector := SelectorFor(t, standardFixture(), "catalog-a", []string{"tool.read"})
				loadError(t, newProvider(t, factory, fixture,
					failure(contracts.PlanningToolCatalogErrorToolConfigInvalid, "")),
					selector, contracts.PlanningToolCatalogErrorToolConfigInvalid)
			})
		}
	})
	t.Run("PL-TC-007 expected evidence mismatch", func(t *testing.T) {
		fixture := standardFixture()
		tests := []struct {
			name string
			edit func(*contracts.PlanningToolCatalogSelector)
		}{
			{name: "registry version", edit: func(selector *contracts.PlanningToolCatalogSelector) {
				selector.ExpectedRegistryVersion = "registry-other"
			}},
			{name: "snapshot hash", edit: func(selector *contracts.PlanningToolCatalogSelector) {
				selector.ExpectedSnapshotHash = contracts.CatalogSnapshotHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				selector := SelectorFor(t, fixture, "catalog-a", []string{"tool.read"})
				test.edit(&selector)
				loadError(t, newProvider(t, factory, fixture,
					failure(contracts.PlanningToolCatalogErrorConfigVersionMismatch, "")),
					selector, contracts.PlanningToolCatalogErrorConfigVersionMismatch)
			})
		}
	})
	t.Run("PL-TC-008 unreadable registry", func(t *testing.T) {
		fixture := standardFixture()
		fixture.Catalogs[0].Unreadable = true
		selector := SelectorFor(t, standardFixture(), "catalog-a", []string{"tool.read"})
		loadError(t, newProvider(t, factory, fixture,
			failure(contracts.PlanningToolCatalogErrorRuntimeFatal, "")),
			selector, contracts.PlanningToolCatalogErrorRuntimeFatal)
	})
	t.Run("PL-TC-009 shared selector independent from agent config", func(t *testing.T) {
		fixture := standardFixture()
		selector := SelectorFor(t, fixture, "catalog-a", []string{"tool.read", "tool.pod"})
		expected := SnapshotFor(t, fixture, "catalog-a", selector.AllowedTools)
		provider := newProvider(t, factory, fixture, success(expected), success(expected))
		agentAHash := contracts.ExecutionConfigHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		agentBHash := contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if agentAHash == agentBHash {
			t.Fatal("fixture requires distinct execution_config_hash values")
		}
		first := loadSuccess(t, provider, selector)
		second := loadSuccess(t, provider, selector)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("same selector returned different snapshots: %+v %+v", first, second)
		}
		validate(t, selector, first)
		validate(t, selector, second)
	})
	t.Run("PL-TC-009A catalog isolation", func(t *testing.T) {
		fixture := standardFixture()
		selectorA := SelectorFor(t, fixture, "catalog-a", []string{"tool.read"})
		selectorB := SelectorFor(t, fixture, "catalog-b", []string{"tool.event"})
		provider := newProvider(t, factory, fixture,
			success(SnapshotFor(t, fixture, "catalog-a", selectorA.AllowedTools)),
			success(SnapshotFor(t, fixture, "catalog-b", selectorB.AllowedTools)),
		)
		snapshotA := loadSuccess(t, provider, selectorA)
		snapshotB := loadSuccess(t, provider, selectorB)
		if len(snapshotA.Tools) != 1 || snapshotA.Tools[0].ToolName != "tool.read" ||
			len(snapshotB.Tools) != 1 || snapshotB.Tools[0].ToolName != "tool.event" ||
			snapshotA.SnapshotHash == snapshotB.SnapshotHash {
			t.Fatalf("Catalog isolation failed: %+v %+v", snapshotA, snapshotB)
		}
	})
	t.Run("PL-TC-010 fixed fixture", func(t *testing.T) {
		fixture := FixedFixture()
		selector := SelectorFor(t, fixture, FixedCatalogID, []string{"k8s.get_pod", "k8s.get_deployment"})
		expectedSnapshot := SnapshotFor(t, fixture, FixedCatalogID, selector.AllowedTools)
		provider := newProvider(t, factory, fixture, success(expectedSnapshot), success(expectedSnapshot))
		snapshot := loadSuccess(t, provider, selector)
		expectedTools := cloneTools(fixture.Catalogs[0].Tools)
		slices.SortFunc(expectedTools, func(left, right contracts.PlanningToolSpec) int {
			if left.ToolName < right.ToolName {
				return -1
			}
			if left.ToolName > right.ToolName {
				return 1
			}
			return 0
		})
		expected := contracts.PlanningToolSnapshot{
			SchemaVersion:   contracts.PlanningToolSnapshotSchemaVersionV1,
			RegistryVersion: FixedRegistryVersion,
			SnapshotHash:    FixedSnapshotHash,
			Tools:           expectedTools,
		}
		if !reflect.DeepEqual(snapshot, expected) {
			t.Fatalf("fixed snapshot fields differ: %+v", snapshot)
		}
		_, payload, err := contracts.ComputePlanningToolSnapshotHash(FixedCatalogID, snapshot)
		if err != nil {
			t.Fatalf("compute fixed snapshot: %v", err)
		}
		if !bytes.Equal(payload, []byte(FixedCanonicalPayload)) {
			t.Fatalf("fixed JCS payload = %s", payload)
		}
		validate(t, selector, snapshot)

		snapshot.Tools[0].Description = "caller mutation"
		again := loadSuccess(t, provider, selector)
		if again.Tools[0].Description == "caller mutation" {
			t.Fatal("Provider returned shared mutable Snapshot state")
		}
	})
	t.Run("PL-TC-011 context cancellation", func(t *testing.T) {
		fixture := standardFixture()
		selector := SelectorFor(t, fixture, "catalog-a", []string{"tool.read"})
		for _, test := range []struct {
			name string
			ctx  func() (context.Context, context.CancelFunc)
			want error
		}{
			{name: "canceled", ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			}, want: context.Canceled},
			{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}, want: context.DeadlineExceeded},
		} {
			t.Run(test.name, func(t *testing.T) {
				ctx, cancel := test.ctx()
				defer cancel()
				expected := SnapshotFor(t, fixture, "catalog-a", selector.AllowedTools)
				provider := newProvider(t, factory, fixture, success(expected))
				snapshot, err := provider.LoadPlanningToolSnapshot(ctx, selector)
				if !errors.Is(err, test.want) || !snapshotIsZero(snapshot) {
					t.Fatalf("canceled call = (%+v, %v), want zero/%v", snapshot, err, test.want)
				}
				if got := loadSuccess(t, provider, selector); !reflect.DeepEqual(got, expected) {
					t.Fatalf("canceled call consumed FIFO response: %+v", got)
				}
			})
		}
	})
	t.Run("PL-TC-012 provider result invariants", func(t *testing.T) {
		fixture := standardFixture()
		valid := SelectorFor(t, fixture, "catalog-a", []string{"tool.read"})
		provider := newProvider(t, factory, fixture,
			success(SnapshotFor(t, fixture, "catalog-a", valid.AllowedTools)),
			failure(contracts.PlanningToolCatalogErrorToolNotFound, "tool.missing"),
		)
		snapshot, err := provider.LoadPlanningToolSnapshot(context.Background(), valid)
		if err != nil || snapshotIsZero(snapshot) {
			t.Fatalf("valid result is not exclusive success: (%+v, %v)", snapshot, err)
		}
		if validationErr := contracts.ValidatePlanningToolSnapshot(valid, snapshot); validationErr != nil {
			t.Fatalf("Provider returned illegal DTO: %v", validationErr)
		}
		missing := SelectorFor(t, fixture, "catalog-a", []string{})
		missing.AllowedTools = []string{"tool.missing"}
		snapshot, err = provider.LoadPlanningToolSnapshot(context.Background(), missing)
		if !snapshotIsZero(snapshot) {
			t.Fatalf("error result contains partial snapshot: %+v", snapshot)
		}
		assertErrorKind(t, err, contracts.PlanningToolCatalogErrorToolNotFound)
	})
}

// FixedFixture 返回 PL-TC-010 跨实现共享的固定 Registry fixture。
func FixedFixture() Fixture {
	return Fixture{Catalogs: []Catalog{{
		CatalogID:       FixedCatalogID,
		RegistryVersion: FixedRegistryVersion,
		Tools: []contracts.PlanningToolSpec{
			tool("k8s.get_pod", "Get one Pod.", contracts.ToolCapabilityK8sGetPod,
				[]string{"cluster", "namespace", "pod"}),
			tool("k8s.get_deployment", "Get one Deployment.", contracts.ToolCapabilityK8sGetDeployment,
				[]string{"cluster", "deployment", "namespace"}),
		},
	}}}
}

// CloneFixture 深拷贝 Provider factory 收到的静态测试事实。
func CloneFixture(input Fixture) Fixture {
	result := Fixture{Catalogs: make([]Catalog, len(input.Catalogs))}
	for index, catalog := range input.Catalogs {
		result.Catalogs[index] = catalog
		result.Catalogs[index].Tools = cloneTools(catalog.Tools)
	}
	return result
}

func standardFixture() Fixture {
	read := tool("tool.read", "Read deployment.", contracts.ToolCapabilityK8sGetDeployment, []string{})
	pod := tool("tool.pod", "Read pod.", contracts.ToolCapabilityK8sGetPod, []string{})
	disabled := tool("tool.disabled", "Disabled event reader.", contracts.ToolCapabilityK8sGetEvent, []string{})
	disabled.Enabled = false
	event := tool("tool.event", "Read event.", contracts.ToolCapabilityK8sGetEvent, []string{})
	return Fixture{Catalogs: []Catalog{
		{CatalogID: "catalog-a", RegistryVersion: "registry-a-v1", Tools: []contracts.PlanningToolSpec{read, pod, disabled}},
		{CatalogID: "catalog-b", RegistryVersion: "registry-b-v1", Tools: []contracts.PlanningToolSpec{event}},
	}}
}

// SelectorFor 计算指定 fixture 投影的冻结 selector 证据。
func SelectorFor(t testing.TB, fixture Fixture, catalogID string, allowed []string) contracts.PlanningToolCatalogSelector {
	t.Helper()
	snapshot := SnapshotFor(t, fixture, catalogID, allowed)
	return contracts.PlanningToolCatalogSelector{
		CatalogID:               catalogID,
		AllowedTools:            slices.Clone(allowed),
		ExpectedRegistryVersion: snapshot.RegistryVersion,
		ExpectedSnapshotHash:    snapshot.SnapshotHash,
	}
}

// SnapshotFor 预生成严格 Fake 的共享 Snapshot；真实 Adapter factory 可忽略该结果。
func SnapshotFor(t testing.TB, fixture Fixture, catalogID string, allowed []string) contracts.PlanningToolSnapshot {
	t.Helper()
	catalog := findCatalog(t, fixture, catalogID)
	selected := make([]contracts.PlanningToolSpec, 0, len(allowed))
	for _, name := range allowed {
		for _, definition := range catalog.Tools {
			if definition.ToolName == name && definition.Enabled {
				selected = append(selected, cloneTool(definition))
				break
			}
		}
	}
	slices.SortFunc(selected, func(left, right contracts.PlanningToolSpec) int {
		if left.ToolName < right.ToolName {
			return -1
		}
		if left.ToolName > right.ToolName {
			return 1
		}
		return 0
	})
	snapshot := contracts.PlanningToolSnapshot{
		SchemaVersion:   contracts.PlanningToolSnapshotSchemaVersionV1,
		RegistryVersion: catalog.RegistryVersion,
		Tools:           selected,
	}
	hash, _, err := contracts.ComputePlanningToolSnapshotHash(catalogID, snapshot)
	if err != nil {
		t.Fatalf("compute selector evidence: %v", err)
	}
	snapshot.SnapshotHash = hash
	return snapshot
}

func newProvider(
	t testing.TB,
	factory Factory,
	fixture Fixture,
	responses ...Response,
) contracts.PlanningToolCatalogPort {
	t.Helper()
	return factory.New(t, Scenario{
		Fixture:   CloneFixture(fixture),
		Responses: cloneResponses(responses),
	})
}

func success(snapshot contracts.PlanningToolSnapshot) Response {
	return Response{Snapshot: cloneSnapshot(snapshot)}
}

func failure(kind contracts.PlanningToolCatalogErrorKind, toolName string) Response {
	var name *string
	if toolName != "" {
		value := toolName
		name = &value
	}
	return Response{Err: contracts.NewPlanningToolCatalogError(
		kind,
		name,
		contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent,
		nil,
	)}
}

func findCatalog(t testing.TB, fixture Fixture, catalogID string) Catalog {
	t.Helper()
	for _, catalog := range fixture.Catalogs {
		if catalog.CatalogID == catalogID {
			return catalog
		}
	}
	t.Fatalf("Catalog fixture %q not found", catalogID)
	return Catalog{}
}

func loadSuccess(
	t testing.TB,
	provider contracts.PlanningToolCatalogPort,
	selector contracts.PlanningToolCatalogSelector,
) contracts.PlanningToolSnapshot {
	t.Helper()
	snapshot, err := provider.LoadPlanningToolSnapshot(context.Background(), selector)
	if err != nil {
		t.Fatalf("LoadPlanningToolSnapshot() error = %v", err)
	}
	if validationErr := contracts.ValidatePlanningToolSnapshot(selector, snapshot); validationErr != nil {
		t.Fatalf("LoadPlanningToolSnapshot() returned invalid DTO: %v", validationErr)
	}
	return snapshot
}

func loadError(
	t testing.TB,
	provider contracts.PlanningToolCatalogPort,
	selector contracts.PlanningToolCatalogSelector,
	want contracts.PlanningToolCatalogErrorKind,
) {
	t.Helper()
	snapshot, err := provider.LoadPlanningToolSnapshot(context.Background(), selector)
	if !snapshotIsZero(snapshot) {
		t.Fatalf("error returned partial snapshot: %+v", snapshot)
	}
	assertErrorKind(t, err, want)
}

func assertErrorKind(t testing.TB, err error, want contracts.PlanningToolCatalogErrorKind) {
	t.Helper()
	var typed *contracts.PlanningToolCatalogError
	if !errors.As(err, &typed) || typed == nil {
		t.Fatalf("error %v is not PlanningToolCatalogError", err)
	}
	if typed.Kind != want || !typed.CauseCode.Valid() {
		t.Fatalf("Catalog error = (%s, %s), want kind %s and safe cause", typed.Kind, typed.CauseCode, want)
	}
}

func snapshotIsZero(snapshot contracts.PlanningToolSnapshot) bool {
	return snapshot.SchemaVersion == 0 && snapshot.RegistryVersion == "" &&
		snapshot.SnapshotHash == "" && snapshot.Tools == nil
}

func cloneSelector(input contracts.PlanningToolCatalogSelector) contracts.PlanningToolCatalogSelector {
	result := input
	result.AllowedTools = slices.Clone(input.AllowedTools)
	return result
}

func tool(
	name string,
	description string,
	kind contracts.ToolCapabilityKind,
	required []string,
) contracts.PlanningToolSpec {
	additional := false
	properties := make(map[string]contracts.CanonicalJSONSchema, len(required))
	for _, property := range required {
		properties[property] = contracts.CanonicalJSONSchema{Type: contracts.JSONSchemaTypeString}
	}
	return contracts.PlanningToolSpec{
		ToolName:    name,
		Description: description,
		InputSchema: contracts.CanonicalJSONSchema{
			Type:                 contracts.JSONSchemaTypeObject,
			Properties:           properties,
			Required:             slices.Clone(required),
			AdditionalProperties: &additional,
		},
		Capability: contracts.PlanningToolCapability{
			Kind: kind, RiskLevel: contracts.RiskLevelLow, ReadOnly: true,
		},
		Enabled: true,
	}
}

func cloneTools(input []contracts.PlanningToolSpec) []contracts.PlanningToolSpec {
	if input == nil {
		return nil
	}
	result := make([]contracts.PlanningToolSpec, len(input))
	for index, definition := range input {
		result[index] = cloneTool(definition)
	}
	return result
}

func cloneResponses(input []Response) []Response {
	if input == nil {
		return nil
	}
	result := make([]Response, len(input))
	for index, response := range input {
		result[index] = response
		result[index].Snapshot = cloneSnapshot(response.Snapshot)
	}
	return result
}

func cloneSnapshot(input contracts.PlanningToolSnapshot) contracts.PlanningToolSnapshot {
	result := input
	result.Tools = cloneTools(input.Tools)
	return result
}

func cloneTool(input contracts.PlanningToolSpec) contracts.PlanningToolSpec {
	result := input
	result.InputSchema = cloneSchema(input.InputSchema)
	return result
}

func cloneSchema(input contracts.CanonicalJSONSchema) contracts.CanonicalJSONSchema {
	result := input
	if input.Items != nil {
		items := cloneSchema(*input.Items)
		result.Items = &items
	}
	if input.Properties != nil {
		result.Properties = make(map[string]contracts.CanonicalJSONSchema, len(input.Properties))
		for name, child := range input.Properties {
			result.Properties[name] = cloneSchema(child)
		}
	}
	result.Required = slices.Clone(input.Required)
	if input.AdditionalProperties != nil {
		additional := *input.AdditionalProperties
		result.AdditionalProperties = &additional
	}
	return result
}

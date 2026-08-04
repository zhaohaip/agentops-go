package contracts

// JSONSchemaType 表示共享受限 JSON Schema 节点类型。
type JSONSchemaType string

const (
	JSONSchemaTypeArray   JSONSchemaType = "array"
	JSONSchemaTypeBoolean JSONSchemaType = "boolean"
	JSONSchemaTypeInteger JSONSchemaType = "integer"
	JSONSchemaTypeNumber  JSONSchemaType = "number"
	JSONSchemaTypeObject  JSONSchemaType = "object"
	JSONSchemaTypeString  JSONSchemaType = "string"
)

// Valid 报告 JSONSchemaType 是否属于受限集合。
func (t JSONSchemaType) Valid() bool {
	switch t {
	case JSONSchemaTypeArray, JSONSchemaTypeBoolean, JSONSchemaTypeInteger,
		JSONSchemaTypeNumber, JSONSchemaTypeObject, JSONSchemaTypeString:
		return true
	default:
		return false
	}
}

// CanonicalJSONSchema 表示共享受限 JSON Schema 的强类型递归值。
//
// 字段按 UTF-8 键序声明，使标准 JSON 编码保持冻结成员顺序。
type CanonicalJSONSchema struct {
	AdditionalProperties *bool                          `json:"additionalProperties,omitempty"`
	Description          string                         `json:"description,omitempty"`
	Items                *CanonicalJSONSchema           `json:"items,omitempty"`
	Nullable             bool                           `json:"nullable,omitempty"`
	Properties           map[string]CanonicalJSONSchema `json:"properties,omitempty"`
	Required             []string                       `json:"required,omitempty"`
	Type                 JSONSchemaType                 `json:"type"`
}

// OutputValueType 表示非 Tool Step 输出字段允许的类型。
type OutputValueType string

const (
	OutputValueTypeArray   OutputValueType = "array"
	OutputValueTypeBoolean OutputValueType = "boolean"
	OutputValueTypeInteger OutputValueType = "integer"
	OutputValueTypeNumber  OutputValueType = "number"
	OutputValueTypeObject  OutputValueType = "object"
	OutputValueTypeString  OutputValueType = "string"
)

// Valid 报告 OutputValueType 是否属于封闭集合。
func (t OutputValueType) Valid() bool {
	return JSONSchemaType(t).Valid()
}

// OutputFieldSchema 表示非 Tool Step 的单个输出字段声明。
type OutputFieldSchema struct {
	Type OutputValueType `json:"type"`
}

// OutputSchema 表示 Planner 与 Step Executor 共用的非 Tool 输出 Schema。
type OutputSchema map[string]OutputFieldSchema

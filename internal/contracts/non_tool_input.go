package contracts

// NonToolInputFieldContract 描述一个冻结的非 Tool Step 顶层输入字段。
type NonToolInputFieldContract struct {
	Name             string
	Type             JSONSchemaType
	Required         bool
	ReferenceAllowed bool
}

// NonToolInputContract 返回 Planner 与 Step Executor 共用的冻结输入字段。
// 返回的 slice 每次独立创建，调用方不能修改全局协议事实。
func NonToolInputContract(stepType StepType) ([]NonToolInputFieldContract, bool) {
	switch stepType {
	case StepTypeModelCall:
		return []NonToolInputFieldContract{
			{Name: "prompt", Type: JSONSchemaTypeString, Required: true, ReferenceAllowed: true},
			{Name: "context", Type: JSONSchemaTypeObject, ReferenceAllowed: true},
		}, true
	case StepTypeAnalysis:
		return []NonToolInputFieldContract{
			{Name: "instruction", Type: JSONSchemaTypeString, Required: true, ReferenceAllowed: true},
			{Name: "evidence", Type: JSONSchemaTypeObject, Required: true, ReferenceAllowed: true},
		}, true
	case StepTypeVerification:
		return []NonToolInputFieldContract{
			{Name: "criteria", Type: JSONSchemaTypeString, Required: true},
			{Name: "evidence", Type: JSONSchemaTypeObject, Required: true, ReferenceAllowed: true},
		}, true
	default:
		return nil, false
	}
}

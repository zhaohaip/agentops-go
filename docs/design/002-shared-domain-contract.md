# AgentOps MVP 跨模块共享领域契约

| 属性 | 值 |
|---|---|
| 文档版本 | V1.1 |
| 文档状态 | MVP 唯一共享契约来源 |
| 需求基线 | `docs/design/001-requirements.md` V3.5 |
| 架构基线 | `docs/design/003-system-architecture-design.md` V1.3 |
| 适用模块 | Task Runtime、Worker、Planner、Step Executor、Tool Framework、Approval、Checkpoint、Report |

本文档只定义被两个及以上模块共同使用的冻结事实。模块内部流程、Repository、持久化实现、事务步骤、私有 DTO、私有状态机和模块专用错误仍由各模块详细设计定义。

规则：

- 本文档中的枚举、字段语义、DTO 字段集合、Port 请求/结果和 Owner 声明是唯一契约来源；
- 模块文档只能描述本模块如何构造、消费或映射共享契约，不得复制一份可独立演进的定义；
- 修改共享契约必须先修改本文档，再同步契约测试和模块使用说明；
- 本文档不覆盖需求或整体架构；发生冲突时先停止整合并记录，不在整理任务中选择新方案；
- 第 10 节列出的未决冲突在确认前不得由实现自行统一。

## 1. 公共状态枚举

### 1.1 TaskStatus

| 值 | 终态 | 语义 |
|---|---:|---|
| `Pending` | 否 | 已创建，尚未开始执行 |
| `Running` | 否 | 当前执行已开始或从已执行位置恢复 |
| `WaitingApproval` | 否 | 等待人工审批 |
| `INTERRUPTED` | 否 | 领取配置失配导致当前 Task 不自动执行，可在配置恢复后人工 Recover |
| `Completed` | 是 | Task 成功完成 |
| `Failed` | 是 | Task 失败终止 |
| `Cancelled` | 是 | User Cancel 或审批拒绝导致取消 |

业务终态集合固定为 `Completed | Failed | Cancelled`。`INTERRUPTED` 不是 Task 终态。

### 1.2 RunStatus

| 值 | 终态 |
|---|---:|
| `Pending` | 否 |
| `Running` | 否 |
| `WaitingApproval` | 否 |
| `Completed` | 是 |
| `Failed` | 是 |

Run 不定义 `Cancelled` 或 `INTERRUPTED`；Task 取消时 Run 使用 `Failed` 和对应 `error_code`。

### 1.3 StepStatus

| 值 | 终态 |
|---|---:|
| `Pending` | 否 |
| `Running` | 否 |
| `WaitingApproval` | 否 |
| `Completed` | 是 |
| `Failed` | 是 |

所有 Step 在执行前都必须经过 `Pending → Running`。Approval Manager 拥有 `Running → WaitingApproval` 和审批决定相关转换；Task Runtime 拥有普通结果转换。

### 1.4 TaskExecutionStatus

| 值 | 执行尝试是否结束 | 语义 |
|---|---:|---|
| `QUEUED` | 否 | 可领取；`worker_id=NULL`、`queued_at` 非空 |
| `RUNNING` | 否 | 已由当前 Runtime Instance 领取 |
| `WAITING_APPROVAL` | 否 | 同一执行尝试暂停并释放 Worker |
| `COMPLETED` | 是 | 本次执行尝试成功 |
| `FAILED` | 是 | 本次执行尝试终止且不可继续或 Recover |
| `INTERRUPTED` | 是 | 本次尝试安全中断；Task Runtime 可以基于有效 Checkpoint 创建新版本 |

`INTERRUPTED` 对当前 TaskExecution 记录是结束状态，但不等于 Task 业务终态。`COMPLETED`、`FAILED`、`INTERRUPTED` 都必须有 `ended_at`。

### 1.5 ToolExecutionStatus

| 值 | 终态 | 语义 |
|---|---:|---|
| `RUNNING` | 否 | Tool 外部调用边界已经持久化 |
| `COMPLETED` | 是 | 已取得明确成功；后处理失败时仍保持 COMPLETED、`output=NULL` |
| `FAILED` | 是 | 已取得明确失败 |
| `UNKNOWN` | 是 | 写 Tool 结果无法确认，必须 `side_effect_unknown=true` |

只读 Tool 不得进入 `UNKNOWN`。`UNKNOWN` 禁止自动重放和 Recover。

### 1.6 ApprovalStatus

`Pending | Approved | Rejected`。`Approved`、`Rejected` 为不可变终态。Task 终态不改写历史 `Pending` Approval。

### 1.7 ReportStatus

| 值 | 终态 |
|---|---:|
| `Pending` | 否 |
| `Generating` | 否 |
| `Completed` | 是 |
| `Failed` | 是 |

`EnsurePending` 对任意已存在合法状态只做幂等确认，不允许把 `Completed` 或 `Failed` 重置为 `Pending`。

## 2. 公共动作与结果枚举

### 2.1 CheckpointNextAction

封闭枚举：

- `GENERATE_PLAN`
- `EXECUTE_STEP`
- `REQUEST_APPROVAL`
- `EXECUTE_APPROVED_TOOL`
- `FINALIZE_RUN`

生成责任：

| 动作 | 生成 Owner |
|---|---|
| `GENERATE_PLAN` | Task 创建/首次领取/合法 Recover 的 Task Runtime 事务 |
| `EXECUTE_STEP` | Task Runtime 的 Planner 结果或 Step 结果事务 |
| `REQUEST_APPROVAL` | Task Runtime 的 Planner/Step 结果事务；合法 Recover 可复制已验证来源 |
| `EXECUTE_APPROVED_TOOL` | Approval Manager Approve 事务；Task Runtime Recover 可复制已验证 Approved 事实 |
| `FINALIZE_RUN` | Task Runtime 最后 Step 结果事务；合法 Recover 可复制 |

目标 Step 的共享生成规则：

| 目标事实 | 结果 |
|---|---|
| ModelCall、Analysis、Verification | `EXECUTE_STEP` |
| Low 且 read_only ToolCall | `EXECUTE_STEP` |
| High 且 write ToolCall，尚无匹配 Approved Approval | `REQUEST_APPROVAL` |
| High 且 write ToolCall，已存在匹配 Approved Approval | `EXECUTE_APPROVED_TOOL` |

Worker、Planner、Step Executor 和 Checkpoint Manager只消费冻结结果，不得运行期重新推导或改写。

### 2.2 StepOutcome

封闭分支：

| 分支 | 共享载荷 | 最小共享语义 |
|---|---|---|
| `Completed` | safe_output、可选 tool_execution_id、tool_result_update、continuation | 已取得安全、确定结果，等待 Task Runtime 提交 |
| `WaitingApproval` | approval_id | Approval Manager 已提交完整等待现场 |
| `Terminalized` | task_id、execution_version、error_code=`CheckpointInvalid`、report_status=`Pending` | Approval Manager 已提交 Task 级 CheckpointInvalid 终态和 Pending Report |
| `Failed` | error_code、safe_summary、可选 tool_execution_id、tool_result_update、side_effect_unknown | 当前动作失败，或写 Tool 结果未知且不可继续 |
| `Stale` | cause_code | 版本、状态或 Worker 所有权已变化，不推进状态 |

成功结果和独立 system error 严格互斥。safe_output、tool_result_update、continuation 等载荷的模块内部结构仍由 Step Executor 定义，但不得改变上述共享字段或增加第六个结果分支。

`Completed.continuation` 只允许 `NEXT_STEP(step_id)` 或 `FINALIZE_RUN`。只要本次动作已经创建 ToolExecution，`Completed` 或 `Failed` 必须携带把它收敛为 `COMPLETED | FAILED | UNKNOWN` 的 `tool_result_update`，禁止遗留 `RUNNING`。

### 2.3 Worker ClaimResult 与 ExecuteResult

`ClaimResult` 封闭分支：

- `Claimed(ExecutionClaim)`
- `NoWork`
- `ConfigMismatchInterrupted`
- `CheckpointInvalidTerminalized`
- `DataInconsistentTerminalized`
- `ExpiredTerminalized`

数据库连接、事务提交不确定、持锁连接异常和无法确定写入目标的 PersistenceInvariantViolation 使用独立 error 通道。

`ExecutionClaim` 固定包含：

- `task_id`
- `run_id`
- `execution_version`
- `worker_id`
- `claimed_at`（领取事务内的 PostgreSQL UTC 时间）

`ExecuteResult` 封闭分支为 `WaitingApproval | Terminal | Stale`。它只表达 Task Runtime 已完成的本轮执行结果；Worker 不解释领域状态，不补写 Task、Report 或 Checkpoint。未知分支、结果与 error 同时存在或空结果加 nil error 都是契约错误。

### 2.4 ReportProcessingResult

封闭分支：`Completed | Failed | NoWork | Interrupted`。Report 自身的 `Failed` 不是 Runtime system error；system error 使用独立 error 通道。

## 3. 错误字段与终止原因

### 3.1 字段语义

| 字段 | 语义 | 禁止事项 |
|---|---|---|
| `error_code` | 已持久化领域对象或确定性命令结果的业务/系统分类 | 不保存自由文本、Provider 原始错误或 `TIMED_OUT` |
| `cause_code` | Port 结果、日志或内部安全诊断的稳定原因分类 | 不替代领域终态，不使用原始错误字符串 |
| `reason_code` | 一个既定 error_code 下的稳定细分原因，例如 CheckpointInvalid | 不扩展状态枚举，不替代 error_code |
| `termination_reason` | TaskExecution 被外部生命周期命令终止的来源 | 只允许本节冻结值，不作为 error_code/cause_code |
| `invariant_code` | `DATA_INCONSISTENT` 下的固定持久化不变量分类 | 不承载 CheckpointInvalid reason |

### 3.2 termination_reason

封闭值：

- `CANCELLED`
- `TIMED_OUT`

User Cancel：Task/Run/活动 Step 使用 `TaskCancelled`，TaskExecution 使用 `termination_reason=CANCELLED`。

Task Timeout：Task/Run/活动 Step 使用 `TaskTimeout`，TaskExecution 使用 `termination_reason=TIMED_OUT`。

`TIMED_OUT` 和 `CANCELLED` 均不得作为领域 `error_code` 或 `cause_code`。

### 3.3 跨模块稳定 error_code/cause_code

以下代码被两个及以上模块共同引用：

| 代码 | 作用域/语义 |
|---|---|
| `TaskCancelled` | Task/Run/Step 取消终态 |
| `TaskTimeout` | Task/Run/Step 超时终态；Port 的 DeadlineExceeded cause |
| `CONFIG_VERSION_MISMATCH` | 领取/Recover 的完整 execution_config_hash 不一致 |
| `DATA_INCONSISTENT` | 可安全归属的持久化跨对象不变量错误 |
| `CheckpointInvalid` | 必须存在的 Checkpoint 缺失或无效 |
| `PlanGenerationFailed` | Planner 未取得可接受候选 |
| `PlanValidationFailed` | 唯一 Repair 后候选仍无效 |
| `InputResolutionFailed` | Step 输入引用解析失败 |
| `ModelCallFailed` | 执行期模型明确失败 |
| `ModelOutputInvalid` | 执行期模型 JSON 解析或 OutputSchema 校验失败 |
| `ResultSanitizationFailed` | 安全脱敏/安全结果处理失败 |
| `StepOutputInvalid` | Tool 已明确成功，但响应读取、解析或 Output Schema 后处理失败 |
| `StepOutputTooLarge` | Tool 已明确成功或有界读取过程中无法安全表达结果 |
| `ToolNotFound` | Tool 不存在的 Step 级业务失败 |
| `ToolDisabled` | Tool 已禁用的 Step 级业务失败 |
| `ToolNotAuthorized` | 当前 Agent 未授权 Tool 的 Step 级业务失败 |
| `ToolInputInvalid` | Tool 参数不满足冻结 Schema 的 Step 级业务失败 |
| `ToolAccessDenied` | Tool 目标超出冻结访问策略的 Step 级业务失败 |
| `ToolTimeout` | 单 Tool 调用超时，不等于 TaskTimeout |
| `ToolConnectionLost` | Tool 连接明确失败 |
| `ToolCallFailed` | Tool 明确业务/Provider 失败 |
| `ApprovalContextChanged` | 冻结资源现场或 resourceVersion 已变化 |
| `ApprovalRejected` | Approval Reject 导致的终态 |
| `WRITE_TOOL_INTERRUPTED` | 写 Tool 结果未知导致执行尝试失败 |
| `WORKER_INTERRUPTED` | Model/只读 Tool 可安全中断 |
| `RESULT_PERSISTENCE_FAILED` | Model/只读 Tool 结果无法安全提交，可 Recover |
| `ReportGenerationFailed` | Report 自身生成失败 |
| `STEP_EXECUTOR_CONTRACT_BROKEN` | Step Executor 与下游共享 DTO/结果契约破坏 |
| `RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT` | 已冻结静态 Tool 投影自相矛盾 |
| `PersistenceInvariantViolation` | 无法安全归属或核心持久化不变量破坏；Runtime Fatal |

Checkpoint 缺失在所有必须存在的 usage 中统一为：

`CheckpointInvalid / reason_code=CHECKPOINT_NOT_FOUND`。

Timeout Port 统一为：

`DeadlineExceeded(cause_code=TaskTimeout)`。

### 3.4 CheckpointInvalid reason_code

以下 reason_code 由 Checkpoint Manager 定义，Task Runtime、Approval、Step Executor 和 Report 只消费或展示安全值：

- `CHECKPOINT_NOT_FOUND`
- `RUNTIME_CONTEXT_MALFORMED`
- `RUNTIME_CONTEXT_VERSION_UNSUPPORTED`
- `CHECKPOINT_ATTRIBUTION_MISMATCH`
- `CHECKPOINT_EXECUTION_HASH_MISMATCH`
- `CHECKPOINT_TYPE_AMBIGUOUS`
- `CHECKPOINT_SOURCE_INVALID`
- `CHECKPOINT_PLAN_REFERENCE_INVALID`
- `CHECKPOINT_STEP_REFERENCE_INVALID`
- `CHECKPOINT_STEP_OUTPUT_REFERENCE_INVALID`
- `CHECKPOINT_REFERENCE_SYNTAX_INVALID`
- `CHECKPOINT_REFERENCE_PATH_INVALID`
- `CHECKPOINT_REFERENCE_PATH_TOO_DEEP`
- `CHECKPOINT_REFERENCE_LIMIT_EXCEEDED`
- `CHECKPOINT_REFERENCE_DUPLICATE_TARGET`
- `CHECKPOINT_REFERENCE_ORDER_INVALID`
- `CHECKPOINT_REFERENCE_MISSING`
- `CHECKPOINT_REFERENCE_EXTRA`
- `CHECKPOINT_REFERENCE_SOURCE_INVALID`
- `CHECKPOINT_APPROVAL_REFERENCE_INVALID`
- `CHECKPOINT_FROZEN_ACTION_MISMATCH`
- `CHECKPOINT_FROZEN_INPUT_HASH_MISMATCH`
- `CHECKPOINT_NEXT_ACTION_INVALID`

这些值不扩展 Task 或 TaskExecution 状态；持久化 Task `error_code` 仍为 `CheckpointInvalid`。

### 3.5 DATA_INCONSISTENT invariant_code

封闭值：

- `CURRENT_EXECUTION_INVALID`
- `QUEUE_STATE_INVALID`
- `CLAIM_SOURCE_AMBIGUOUS`
- `CROSS_OBJECT_STATE_INVALID`

## 4. ExecutionScope

`ExecutionScope` 是进程内执行关联的唯一共享 DTO；Task Runtime 是唯一构造 Owner。

```go
type ExecutionVersion int64
type ExecutionConfigHash string

type ExecutionScope struct {
	TaskID              TaskID
	RunID               RunID
	ExecutionVersion    ExecutionVersion
	ExecutionConfigHash ExecutionConfigHash
	WorkerID            WorkerID
	StepID              StepID
	DeadlineAt          time.Time
}
```

| 字段 | 约束与事实来源 |
|---|---|
| task_id、run_id、step_id | 已重新锁定并验证的当前执行链 |
| execution_version | 大于0；当前 TaskExecution 版本 |
| execution_config_hash | 64位小写十六进制；来自已通过三方门禁的 TaskExecution |
| worker_id | 当前 Runtime Instance 对 TaskExecution 的所有权 |
| deadline_at | 持久化 Task 的 PostgreSQL UTC deadline |

Task Runtime只复制已验证事实，不在Scope构造时重算hash。Step Executor、Tool Framework、Approval和Checkpoint不得补全、刷新或修改Scope。Scope不能替代数据库Version/Ownership/State Guard。

`ExecutionVersion` 是所有表达持久化 `execution_version` 概念的唯一共享领域类型：

- 底层类型固定为 `int64`，与 PostgreSQL `BIGINT` 对齐；
- 合法持久化值从 1 开始，零值和负值非法；
- 当前版本、来源版本、Approval 所属版本、Checkpoint 所属版本、ExecutionClaim、Command/Result DTO、Repository DTO 和领域实体必须使用同一类型；
- 可空来源版本使用 `*ExecutionVersion` 或语义等价的显式 Optional，不得退回 `*int64`；
- 只有数据库 Adapter 可以在 PostgreSQL `BIGINT` 与 `ExecutionVersion` 之间转换，并必须拒绝零值、负值及越界值；
- `version+1` 必须在创建新 TaskExecution 的事务前检查 `math.MaxInt64` 上界，禁止溢出或回绕；
- 禁止使用 `uint64`、裸 `int64`、`int` 或字符串表达同一领域字段，也禁止跨模块隐式数值转换。

## 5. ExecutionConfigV1 与 execution_config_hash

### 5.1 唯一 Owner

Task Runtime唯一负责：

1. 从已通过启动校验的静态配置构造 `ExecutionConfigV1`；
2. 应用共享强类型默认值；
3. 校验字段、版本、排序和空值；
4. 生成规范化 JSON；
5. 计算、持久化和比较完整 `execution_config_hash`；
6. 从同一不可变实例投影各模块只读配置事实。

其他模块不得计算完整hash、追加本地输入字段、维护第二套默认值/排序/JSON编码器，或从hash反推完整配置。按冻结Guard/Port契约比较两个既有hash证据是允许的。

### 5.2 顶层结构

```go
type ExecutionConfigV1 struct {
	Schema        string
	Version       uint32
	Agent         AgentExecutionConfigV1
	Model         ModelExecutionConfigV1
	JSON          JSONExecutionContractV1
	Safety        SafetyExecutionContractV1
	Planner       PlannerExecutionConfigV1
	StepExecutor  StepExecutorExecutionConfigV1
	ToolFramework ToolFrameworkExecutionConfigV1
	Checkpoint    CheckpointExecutionConfigV1
	Approval      ApprovalExecutionConfigV1
}
```

`schema="agentops.execution-config"`，`version=1`。全部结构字段必填且禁止未知字段和 `null`。

按规范化输出顺序的子字段：

| 对象 | 字段 |
|---|---|
| agent | agent_id、enabled、system_instruction、allowed_tools、max_steps |
| model | model_name、stream、response_format、model_client_contract_version、generation_params_schema_version、generation_params |
| generation_params | temperature、top_p、max_output_tokens |
| json | canonicalization_version、max_depth、max_object_fields、reject_duplicate_keys、reject_null |
| safety | sanitization_rule_version、safe_summary_max_bytes、log_string_max_bytes |
| planner | contract_version、plan_schema_version、non_tool_input_contract_version、tool_schema_subset_version、repair_policy_version、allowed_step_types、final_step_type、sequence_start、requires_contiguous_sequence、max_repairs、limits |
| step_executor | contract_version、step_input_contract_version、reference_protocol_version、reference_action_mode_version、output_schema_version、limits |
| tool_framework | contract_version、result_contract_version、tools、access_policy、result_limits、event_policy、patch_policy |
| checkpoint | contract_version、runtime_context_schema_version、resolved_reference_protocol_version、action_mode_version、max_resolved_references_per_step、max_target_path_depth |
| approval | policy_version、required_risk_level、required_read_only、freeze_resource_version |

详细限制结构字段顺序固定为：

| 对象 | 按顺序排列的字段 |
|---|---|
| PlannerLimitsV1 | max_task_input_bytes、max_agent_prompt_bytes、max_tool_description_bytes、max_tool_schema_bytes、max_planning_tools、max_initial_prompt_bytes、max_repair_prompt_bytes、max_model_response_bytes、max_plan_steps、max_plan_draft_bytes、max_step_name_bytes、max_goal_bytes、max_step_input_bytes、max_resolved_references_per_step、max_output_fields、max_output_field_name_bytes、max_validation_issues、max_repair_candidate_summary_bytes、planner_model_call_timeout_ms、repair_min_model_budget_ms、planner_local_safety_margin_ms；全部为uint64 |
| StepExecutorLimitsV1 | max_resolved_step_input_bytes、max_step_output_bytes、max_model_prompt_bytes、max_model_response_bytes、max_resolved_references_per_step、max_target_path_depth；全部为uint64 |
| ToolDefinitionV1 | name、enabled、description、capability_kind、input_schema、output_schema、risk_level、read_only、timeout_ms |
| ToolAccessPolicyV1 | clusters、replicas_policy、image_registry_allowlist |
| ClusterPolicyV1 | cluster_id、namespaces、resources |
| ResourcePolicyV1 | kind、verbs、write_fields |
| ReplicasPolicyV1 | enabled、min、max；disabled时min=0、max=0 |
| ToolResultLimitsV1 | raw_response_max_bytes、safe_dto_max_bytes、pod_page_limit、event_page_limit、container_log_default_lines、container_log_max_lines |
| EventPolicyV1 | version、sort_keys、candidate_budget_bytes、reserve_bytes、follow_continue |
| PatchPolicyV1 | version、response_classification_version、resource_version_test_required、allowed_write_fields |

### 5.3 GenerationParams

```text
temperature       CanonicalDecimalV1  default 0.2  range [0,2]
top_p             CanonicalDecimalV1  default 1    range (0,1]
max_output_tokens uint32              default 4096 range [1,8192]
```

Task Runtime配置加载器唯一负责默认值和规范化。Planner、Step Executor、Report和Model Adapter只消费规范化值；Adapter不得以二进制浮点值反向生成hash。

### 5.4 规范化和Hash

- 允许的集合使用 `[]`，不得使用 `null`；
- 字符串必须是合法UTF-8，不trim、不做Unicode归一化；
- 无业务顺序的集合去重并按UTF-8字节排序；Tool按name、cluster按cluster_id、resource按kind排序；
- event sort_keys保留业务顺序；
- JSON使用UTF-8、无BOM、无缩进、无无关空白、无结尾换行；
- bool使用true/false；整数无前导零；decimal使用最短无损非指数表示，禁止NaN、Infinity和负零；
- hash输入是完整规范化ExecutionConfigV1字节；
- 算法固定SHA-256，输出64个ASCII小写十六进制字符，无`sha256:`前缀；
- 比较为完整字符串精确比较，不做大小写转换、截断或Base64转换；
- API Key、Token、endpoint、凭证、日志级别、advisory lock和关闭宽限期不进入hash。

固定测试向量的规范化JSON如下。代码块内容无BOM、无结尾换行：

```json
{"schema":"agentops.execution-config","version":1,"agent":{"agent_id":"agent-default","enabled":true,"system_instruction":"You are AgentOps.","allowed_tools":["k8s.get_deployment"],"max_steps":20},"model":{"model_name":"deepseek-chat","stream":false,"response_format":"json_object","model_client_contract_version":"model-client-v1","generation_params_schema_version":1,"generation_params":{"temperature":0.2,"top_p":1,"max_output_tokens":4096}},"json":{"canonicalization_version":"agentops-json-v1","max_depth":16,"max_object_fields":64,"reject_duplicate_keys":true,"reject_null":true},"safety":{"sanitization_rule_version":"result-sanitization-v1","safe_summary_max_bytes":512,"log_string_max_bytes":256},"planner":{"contract_version":"planner-v1.3","plan_schema_version":1,"non_tool_input_contract_version":"non-tool-input-v1","tool_schema_subset_version":"tool-schema-subset-v1","repair_policy_version":"single-repair-v1","allowed_step_types":["Analysis","ModelCall","ToolCall","Verification"],"final_step_type":"Verification","sequence_start":1,"requires_contiguous_sequence":true,"max_repairs":1,"limits":{"max_task_input_bytes":16384,"max_agent_prompt_bytes":32768,"max_tool_description_bytes":4096,"max_tool_schema_bytes":65536,"max_planning_tools":32,"max_initial_prompt_bytes":262144,"max_repair_prompt_bytes":393216,"max_model_response_bytes":1048576,"max_plan_steps":20,"max_plan_draft_bytes":262144,"max_step_name_bytes":128,"max_goal_bytes":2048,"max_step_input_bytes":32768,"max_resolved_references_per_step":256,"max_output_fields":32,"max_output_field_name_bytes":64,"max_validation_issues":32,"max_repair_candidate_summary_bytes":65536,"planner_model_call_timeout_ms":60000,"repair_min_model_budget_ms":15000,"planner_local_safety_margin_ms":2000}},"step_executor":{"contract_version":"step-executor-v1","step_input_contract_version":"step-input-v1","reference_protocol_version":"step-output-ref-v1","reference_action_mode_version":"reference-action-mode-v1","output_schema_version":"output-schema-v1","limits":{"max_resolved_step_input_bytes":1048576,"max_step_output_bytes":1048576,"max_model_prompt_bytes":262144,"max_model_response_bytes":1048576,"max_resolved_references_per_step":256,"max_target_path_depth":16}},"tool_framework":{"contract_version":"tool-framework-v1","result_contract_version":"tool-framework-result-v1","tools":[{"name":"k8s.get_deployment","enabled":true,"description":"Get one Deployment.","capability_kind":"K8S_GET_DEPLOYMENT","input_schema":{"additionalProperties":false,"properties":{"cluster":{"type":"string"},"deployment":{"type":"string"},"namespace":{"type":"string"}},"required":["cluster","deployment","namespace"],"type":"object"},"output_schema":{"additionalProperties":false,"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"},"risk_level":"Low","read_only":true,"timeout_ms":30000}],"access_policy":{"clusters":[{"cluster_id":"prod","namespaces":["default"],"resources":[{"kind":"Deployment","verbs":["get"],"write_fields":[]}]}],"replicas_policy":{"enabled":false,"min":0,"max":0},"image_registry_allowlist":[]},"result_limits":{"raw_response_max_bytes":1048576,"safe_dto_max_bytes":1048576,"pod_page_limit":200,"event_page_limit":200,"container_log_default_lines":200,"container_log_max_lines":1000},"event_policy":{"version":"bounded-event-page-v1","sort_keys":["event_time_desc","namespace_asc","name_asc","uid_asc"],"candidate_budget_bytes":983040,"reserve_bytes":65536,"follow_continue":false},"patch_policy":{"version":"deployment-patch-v1","response_classification_version":"patch-final-status-v1","resource_version_test_required":true,"allowed_write_fields":["image","replicas"]}},"checkpoint":{"contract_version":"checkpoint-v1.3","runtime_context_schema_version":1,"resolved_reference_protocol_version":"step-output-ref-v1","action_mode_version":"checkpoint-action-mode-v1","max_resolved_references_per_step":256,"max_target_path_depth":16},"approval":{"policy_version":"approval-policy-v1","required_risk_level":"High","required_read_only":false,"freeze_resource_version":true}}
```

期望SHA-256：

`27f26b3d37da75facddaca5b9cbc23de91093977d75746fb5c53a0d92d257f43`

八个模块不得各自维护不同fixture。

### 5.5 三方门禁

- 首次领取：当前ExecutionConfigV1、TaskExecution、Initialization Checkpoint；
- 非首次领取：当前ExecutionConfigV1、TaskExecution、当前版本最新有效Checkpoint；
- Recover：当前ExecutionConfigV1、来源TaskExecution、恢复来源Checkpoint；
- 三方必须在创建新版本或执行外部调用前完全相等；
- 失配时原hash不变，observed_config_hash首次保存当前配置hash；
- Tool授权仍必须在调用时独立校验，hash一致不替代RBAC和静态权限检查。

### 5.6 Planning Tool Catalog 独立证据

完整execution_config_hash与Catalog证据分离。Catalog不接收、不保存、不比较完整hash。

```go
type CatalogSnapshotHash string

type PlanningToolCatalogPort interface {
	LoadPlanningToolSnapshot(
		ctx context.Context,
		selector PlanningToolCatalogSelector,
	) (PlanningToolSnapshot, error)
}

type PlanningToolCatalogSelector struct {
	CatalogID               string
	AllowedTools            []string
	ExpectedRegistryVersion string
	ExpectedSnapshotHash    CatalogSnapshotHash
}

type PlanningToolSnapshot struct {
	SchemaVersion   uint32
	RegistryVersion string
	SnapshotHash    CatalogSnapshotHash
	Tools           []PlanningToolSpec
}

type PlanningToolSpec struct {
	ToolName    string
	Description string
	InputSchema CanonicalJSONSchema
	Capability  PlanningToolCapability
	Enabled     bool
}

type PlanningToolCapability struct {
	Kind      ToolCapabilityKind
	RiskLevel RiskLevel
	ReadOnly  bool
}
```

`CatalogSnapshotHash`与`ExecutionConfigHash`是不同强类型。Catalog hash覆盖schema_version、catalog_id、registry_version及按tool_name排序的选定Tool投影，使用RFC8785-JCS、SHA-256和小写十六进制。Tool Framework是Catalog hash Owner；Task Runtime只投影启动时冻结的selector；Planner只验证selector与snapshot。

Selector 不包含 `agent_id`、完整 `execution_config_hash`、执行版本或运行时凭证。成功 Snapshot 必须满足：

- `schema_version=1`；
- tools 与 `allowed_tools` 集合精确相等，按 `tool_name` Unicode code point 升序且无重复；
- 每项包含稳定名称、安全描述、规范化 Input Schema、capability 和 `enabled=true`；
- selector 的预期 Registry 版本和 Snapshot hash 与返回证据一致；
- 不返回部分结果。

错误使用独立 error 通道并可由 `errors.As` 得到以下封闭 kind：

`ToolNotFound | ToolDisabled | DuplicateTool | ToolConfigInvalid | ConfigVersionMismatch | RuntimeFatal`。

`context.Canceled` 和 `context.DeadlineExceeded` 必须保留 `errors.Is` 语义，不改写为 Catalog 配置错误。

## 6. 共享 Model Client Port

```go
type ModelClient interface {
	GenerateStructured(
		ctx context.Context,
		request ModelRequest,
	) (ModelResponse, error)
}
```

公共请求约束：

- model固定`deepseek-chat`；
- stream固定false；
- response_format固定`json_object`；
- generation_params使用第5.3节共享值；
- metadata只用于安全调用关联，不进入模型消息；
- 不包含数据库事务、Provider SDK类型或凭证。

ModelResponse只公开assistant_content和可选的进程内provider_request_id。ModelClientError封闭类别：

`Canceled | Timeout | Authentication | Network | RateLimited | Provider | ResponseTooLarge | InvalidResponse | ContractViolation`。

Adapter不自动重试、Fallback或切换模型，不记录完整Prompt/响应，不向应用层暴露Eino/Provider原始类型。

## 7. Tool、Approval 与 Checkpoint 共享契约

### 7.1 Tool Framework Execution Port

公开执行Port只有：

```go
type ToolFrameworkPort interface {
	InvokeReadTool(context.Context, ReadToolRequest) (ToolFrameworkResult, error)
	PrepareWriteApproval(context.Context, PrepareWriteApprovalRequest) (ToolFrameworkResult, error)
	InvokeApprovedWrite(context.Context, ApprovedWriteRequest) (ToolFrameworkResult, error)
}
```

请求DTO字段集合：

| DTO | 字段 |
|---|---|
| ReadToolRequest | Scope、Authorization、ToolName、ResolvedInput、ToolDefinition |
| PrepareWriteApprovalRequest | Scope、Authorization、ToolName、ResolvedInput、ToolDefinition |
| ApprovedWriteRequest | Scope、Authorization、ApprovedAction、CheckpointEvidence、ToolDefinition |

辅助共享 DTO：

| DTO | 冻结字段/语义 |
|---|---|
| AgentAuthorization | agent_id、allowed_tools；Task Runtime 从计算当前 hash 的同一 ExecutionConfigV1.agent 投影 |
| StaticToolDefinition | 对应 ExecutionConfigV1.tool_framework.tools 中同名 Tool 的 name、enabled、description、capability_kind、input_schema、output_schema、risk_level、read_only、timeout_ms 投影 |
| FrozenToolRequest | task/run/execution_version/step/tool 标识、完整规范输入、目标定位、允许修改的旧值和新值、resource_version、安全摘要、execution_config_hash、frozen_input_hash |

`FrozenToolRequest` 不包含完整 Kubernetes 对象、凭据、Secret、managedFields 或白名单外 metadata。其 `execution_config_hash` 从 Scope 原样复制；`frozen_input_hash` 使用共享 `FrozenApprovedToolInputV1` 规范函数计算。Task Runtime、Step Executor、Tool Framework 和 Approval 不得维护不同字段版本。

ToolFrameworkResult封闭分支：

`InvocationCompleted | ApprovalPrepared | PreflightRejected | ToolBusinessFailed | SideEffectUnknown | CheckpointInvalid | DeadlineExceeded | Stale | RuntimeFatal`。

分支共享载荷：

| 分支 | 字段 |
|---|---|
| InvocationCompleted | tool_execution_id、安全 output、truncated、可选 original_size/original_count、可选 processing_error |
| ApprovalPrepared | FrozenToolRequest |
| PreflightRejected | error_code、safe_summary |
| ToolBusinessFailed | error_code、safe_summary、可选 tool_execution_id、可选 tool_execution_status=`FAILED` |
| SideEffectUnknown | tool_execution_id、error_code、safe_summary、side_effect_unknown=`true` |
| CheckpointInvalid | reason_code |
| DeadlineExceeded | cause_code=`TaskTimeout` |
| Stale | reason_code、可选 tool_execution_id |
| RuntimeFatal | error_code、safe_cause_code |

方法允许分支：

| 方法 | 允许结果 |
|---|---|
| InvokeReadTool | InvocationCompleted、ToolBusinessFailed、DeadlineExceeded、Stale、RuntimeFatal |
| PrepareWriteApproval | ApprovalPrepared、PreflightRejected、ToolBusinessFailed、DeadlineExceeded、Stale、RuntimeFatal |
| InvokeApprovedWrite | InvocationCompleted、PreflightRejected、ToolBusinessFailed、SideEffectUnknown、CheckpointInvalid、DeadlineExceeded、Stale、RuntimeFatal |

`ValidateCapability`不是公开Port。结果与error必须互斥。

### 7.2 ApprovedAction 与 ApprovedCheckpointEvidence

ApprovedAction只来自不可变Approval：

- approval_id
- approval_execution_version
- approval_status（固定Approved）
- execution_config_hash
- frozen_input_hash
- task_id、run_id、step_id、tool_name
- frozen_input、observed_values、resource_version

ApprovedCheckpointEvidence只来自当前版本最新有效Checkpoint：

- checkpoint_id
- approval_id
- execution_version
- checkpoint_type（`APPROVED_CONTINUATION | RECOVERY_START`）
- source_execution_version、source_checkpoint_id
- execution_config_hash
- frozen_input_hash

同版本路径source字段均空；Recovery路径source字段同时存在。两个DTO必须满足approval_id、frozen_input_hash和适用hash证据一致。Checkpoint不能复制完整Approval动作，Approval不能推导Checkpoint来源。

所有 execution_version 字段使用第4节共享 `ExecutionVersion`；可空 source_execution_version 使用 `*ExecutionVersion`。

`frozen_input_hash` 的唯一输入协议为
`FrozenApprovedToolInputV1{tool_name,tool_input,observed_values,resource_version}`。使用 AgentOps 规范 JSON、SHA-256 和 64 位小写十六进制编码。共享纯函数只计算此摘要，不读取数据库或配置。

固定规范字节（无 BOM、空白或结尾换行）：

```json
{"schema":"agentops.frozen-approved-tool-input","version":1,"tool_name":"k8s.patch_deployment","tool_input":{"cluster":"prod","deployment":"web","namespace":"default","replicas":3},"observed_values":{"replicas":2},"resource_version":"12345"}
```

期望摘要：

`c33d13c983cc54ab1c906c40004b9c2a3ca2efba506ae8db4a12ddca1f4c70f4`

### 7.3 Approval Request Port

```go
type ApprovalRequestPort interface {
	RequestApproval(
		ctx context.Context,
		command RequestApprovalCommand,
	) (ApprovalRequestResult, error)
}
```

RequestApprovalCommand字段：

- Scope
- FrozenRequest
- StepID
- ExecutionConfigHash
- ApprovalContext

`ApprovalContext` 的共享类型为 `ApprovalRequestContext`，字段固定为：

- `NextAction`（固定 `REQUEST_APPROVAL`）
- `ToolName`
- `RiskLevel`（固定 `High`）
- `ReadOnly`（固定 `false`）

ApprovalRequestResult封闭分支：

`Pending | Existing | Conflict | CheckpointInvalid | RuntimeFatal`。

分支最小载荷：

| 分支 | 字段 |
|---|---|
| Pending | approval_id、approval_status、task_id、run_id、step_id、execution_version |
| Existing | approval_id、approval_status、task_id、run_id、step_id、execution_version |
| Conflict | task_id、execution_version、cause_code |
| CheckpointInvalid | task_id、run_id、step_id、execution_version、error_code、reason_code、task_execution_status、report_status |
| RuntimeFatal | error_code、cause_code、可选 task_id、可选 step_id |

`CheckpointInvalid` 分支中的 `error_code=CheckpointInvalid`、`task_execution_status=FAILED`、`report_status=Pending`。所有分支中的 execution_version 字段使用第4节共享 `ExecutionVersion`。

Timeout固定返回`Conflict(cause_code=TaskTimeout)`。结果与error必须互斥。

### 7.4 Checkpoint共享DTO

RuntimeContextV1、CanonicalResolvedReferences、ResolvedReference、ApprovalContext、ApprovedCheckpointEvidence及CheckpointNextAction是Task Runtime、Step Executor、Approval和Checkpoint Manager共享的只读契约。

`RuntimeContextV1` 字段固定为：

| 字段 | 必填条件 |
|---|---|
| schema_version（固定1）、task_id、run_id、execution_version、next_action、resolved_references | 始终 |
| plan_id | Plan 已生成后 |
| current_step_id | next_action 与 Step 有关时 |
| approval_context | WaitingApproval 或执行 Approved Tool 时 |

`ResolvedReference` 字段固定为：

- `target_path`
- `source_step_id`
- `source_output_field`

`target_path` 是 segment 数组；segment 只允许 `{kind:"key",key:string}` 或 `{kind:"index",index:non-negative integer}`。禁止空路径、未知字段、JSONPath/JMESPath 字符串或同时携带 key/index。

ResolvedReference协议：

- Task Runtime是持久化列表唯一构造Owner；
- Checkpoint Manager只校验和保存；
- TARGET_STEP_INPUT用于EXECUTE_STEP、REQUEST_APPROVAL、EXECUTE_APPROVED_TOOL；
- NO_STEP_INPUT用于GENERATE_PLAN、FINALIZE_RUN，引用列表固定为空；
- 单Step最多256条、target_path最大深度16；
- 排序、去重和target_path线协议使用共享引用提取器；
- 普通文本中部出现`step.output.`按字面量处理。

共享提取器的数量超限 issue 固定为 `REFERENCE_COUNT_LIMIT_EXCEEDED`：Planner 将其作为 ValidationIssue，Step Executor 将其作为契约 cause，Checkpoint Manager 确定性映射为 `CheckpointInvalid/CHECKPOINT_REFERENCE_LIMIT_EXCEEDED`。

Checkpoint Manager的公共验证结果是：

`ValidCheckpoint | CheckpointInvalid(reason_code) | PersistenceInvariantViolation`。

- `ValidCheckpoint` 携带已验证的 Checkpoint、Runtime Context、推断类型和持久化 `execution_config_hash`；
- `CheckpointInvalid` 只用于对象可安全归属但必须存在的 Checkpoint 缺失、内容无效或引用无效；
- `PersistenceInvariantViolation` 只用于无法唯一确定 Task/Run/Execution 或安全写入目标的 Runtime Fatal；
- `Stale`、状态冲突和 deadline 由调用方的共享 Guard 判定，不属于 Checkpoint Manager 的返回联合类型；
- 数据库连接、事务提交不确定等基础设施故障使用独立 system error 通道。

### 7.5 Step 输入与输出 Schema

Planner 与 Step Executor 共用同一 `OutputSchema`：

- 顶层为非空 object；
- 字段名匹配 `^[A-Za-z_][A-Za-z0-9_]*$`，区分大小写；
- 每个字段描述只能包含必填 `type`；
- type 只允许 `string | number | integer | boolean | object | array`；
- 不允许 nullable、required、properties、items、description 或未知关键字；
- object/array 只能作为完整直接字段引用，不支持多级输出路径或数组下标；
- 运行期 null 不满足任一声明类型。

非 Tool Step 的输入契约：

| Step类型 | 字段 |
|---|---|
| ModelCall | prompt：必填非空string、可引用string；context：可选object、可引用object |
| Analysis | instruction：必填非空string、可引用string；evidence：必填object、可引用object |
| Verification | criteria：必填非空静态string、不可引用；evidence：必填object、可引用object |

这些输入禁止额外顶层字段和 null。Planner负责静态语法与声明类型校验；Step Executor 在替换引用后按同一契约验证实际值。

Tool `input_schema` 共用以下受限关键字：

- `type`：必填单字符串，只允许 object、array、string、number、integer、boolean；
- `properties`、`required`、`additionalProperties` 仅按 object 规则使用，additionalProperties 省略等价 false，true 或 Schema 形式不支持；
- array 必须有单个 `items` 子 Schema；
- `nullable` 可省略或为 bool，默认 false，顶层 Tool input 不可 nullable；
- `description` 是可选模型可见字符串；
- 禁止 `$ref`、组合 Schema、条件 Schema、type 数组、patternProperties 和动态 additional properties；
- integer 可赋给 number，其他引用类型必须相同。

Schema 版本由 `ExecutionConfigV1.planner.tool_schema_subset_version`、`non_tool_input_contract_version` 和 `step_executor.output_schema_version` 冻结。Planner、Step Executor 与 Tool Framework 不得局部扩展。

## 8. 共享事务与写入 Port

### 8.1 RuntimeWriteExecutor

所有领域写使用持有PostgreSQL advisory lock的同一连接和短事务。`RuntimeWriteTx`是AgentOps事务能力，不是`*sql.Tx`；下游Port不得提交、回滚、缓存或跨调用保存它。事务中禁止LLM、Kubernetes及其他长耗时外部调用。

```go
type RuntimeWriteExecutor interface {
	Execute(
		ctx context.Context,
		work func(context.Context, RuntimeWriteTx) error,
	) error
}

type RuntimeWriteTx interface {
	AgentOpsRuntimeWriteTx()
}
```

`RuntimeWriteTx` 只是不透明事务令牌，不包含 SQL、Repository、数据库连接、提交或回滚方法。事务 Owner 只依赖本共享 Port；各 Repository Port 接收同一个令牌，由具体 PostgreSQL Repository Adapter 在基础设施边界内验证并使用其对应事务。业务模块不得导入 PostgreSQL Adapter、断言具体令牌类型或取得 `pgx.Tx`。

### 8.2 PendingReportWriter

```go
type PendingReportWriter interface {
	EnsurePending(
		ctx context.Context,
		tx RuntimeWriteTx,
		request EnsurePendingReportRequest,
	) (EnsurePendingReportResult, error)
}
```

EnsurePendingReportRequest字段：

- task_id
- run_id
- created_at（调用方事务内PostgreSQL UTC时间）

结果封闭为`Created | Existing`。Report模块拥有`UNIQUE(task_id)`、冲突复用、run_id一致性和新行初始化规则。Task Runtime和Approval Manager必须复用当前事务，不得直接写Report表。

### 8.3 Task Lifecycle Policy

Task Lifecycle Policy是无状态纯规则组件，不是服务或持久化对象。Task Runtime定义Task生命周期规则；Approval Manager只在其事务中调用共享Policy执行审批命令导致的转换。Checkpoint Manager、Worker、Planner、Step Executor、Tool Framework和Report不得自行定义Task生命周期。

共享规则方法：

| 方法 | 输入事实 | 结果 |
|---|---|---|
| CanEnterWaitingApproval | Task、Run、Step、TaskExecution、current_execution_version、worker_id、deadline、db_now | Allowed 或稳定拒绝原因 |
| CanApprove | Approval、Task、Run、Step、TaskExecution、current_execution_version、deadline、db_now | Allowed 或稳定拒绝原因 |
| CanReject | Approval、Task、Run、Step、TaskExecution、current_execution_version、deadline、db_now | Allowed 或稳定拒绝原因 |
| CanTerminalizeCheckpointInvalid | source、上述锁定事实、request_execution_version、入口预期状态、deadline、db_now | Allowed 或稳定拒绝原因 |

CheckpointInvalid 的 `source` 只允许 `RequestApproval | Approve | Reject`。Policy 不读取数据库、不生成时间、不写 Checkpoint/Report，也不判断 Checkpoint 内容；Approval Manager 必须先完成版本、所有权、状态、deadline 和 DTO Guard，Checkpoint Manager 再确认可安全归属但无效，最后才调用该 Policy。

## 9. 公共Owner与校验责任

| 共享事实 | 唯一Owner/构造者 | 其他模块责任 |
|---|---|---|
| Task生命周期规则 | Task Runtime / Task Lifecycle Policy | 只调用或消费，不复制状态规则 |
| ExecutionConfigV1与execution_config_hash | Task Runtime | 只引用、格式校验或按既有Guard比较 |
| ExecutionScope | Task Runtime | 原样传递、交叉校验，不补全 |
| current_execution_version | Task表，由Task Runtime事务推进 | 所有写携带版本Guard |
| Worker Ownership | Task Runtime Claim事务 | Worker只消费Claim |
| next_action普通Step | Task Runtime结果事务 | 下游只消费 |
| next_action审批继续 | Approval Manager Approve事务 | 下游只消费 |
| Planning Tool Registry/Catalog hash | Tool Framework | Planner验证，Task Runtime投影selector |
| ToolExecution RUNNING边界 | Tool Framework | Task Runtime提交最终结果 |
| Approval及审批决定 | Approval Manager | Step Executor调用并解释封闭结果 |
| Checkpoint校验/保存 | Checkpoint Manager | Task Runtime构造恢复策略与resolved_references |
| resolved_references | Task Runtime + 共享提取器 | Checkpoint校验；Step Executor解析值 |
| Pending Report幂等规则 | Report模块 | Task Runtime/Approval通过Port调用 |
| Report内容和状态 | Report Manager | 其他模块只创建Pending或读取 |
| Model Client错误分类 | Model Adapter | Planner/Step/Report映射，不解析字符串 |

## 10. 已确认的契约统一决策

### 10.1 execution_version 领域类型

统一使用第4节唯一声明的 `ExecutionVersion`（底层类型 `int64`）。

以下字段全部使用该类型：

- `ExecutionScope.ExecutionVersion`；
- `ExecutionClaim.ExecutionVersion`；
- `Task.current_execution_version` 和 `TaskExecution.execution_version` 的应用层字段；
- `ApprovedAction.ApprovalExecutionVersion`；
- `ApprovedCheckpointEvidence.ExecutionVersion`；
- `ApprovedCheckpointEvidence.SourceExecutionVersion` 使用 `*ExecutionVersion`；
- Approval Request/Command Result、Checkpoint、Recover、StartupCleanup、ReportFacts 和 Repository DTO 中的版本字段。

PostgreSQL 仍使用 `BIGINT`；本决策只统一 AgentOps Go 领域与 Port 类型，不改变数据库列语义。当前没有未解决的 `execution_version` 类型冲突。

共享契约测试必须覆盖：

- `ExecutionScope`、ExecutionClaim、ApprovedAction、ApprovedCheckpointEvidence、Approval Result、Checkpoint/Recover DTO 与 ReportFacts 在编译期使用同一 `ExecutionVersion`；
- Repository Adapter 从 `BIGINT` 扫描 1 和 `math.MaxInt64` 成功，扫描 0 或负值返回持久化不变量错误；
- 可空 `source_execution_version` 只允许 nil 或合法 `ExecutionVersion`；
- Recover 创建 `version+1` 时对 `math.MaxInt64` 拒绝溢出，不创建 TaskExecution、Checkpoint 或 queued_at；
- Fake、Mock 和真实 Adapter 使用相同类型，不提供接收 `uint64`、裸 `int64`、`int` 或字符串的兼容重载。

## 11. 明确保留在模块内部的设计

以下内容不属于共享契约，不得因为本文档的建立而迁移或抽象为新的跨模块接口：

| 模块 | 保留内容 |
|---|---|
| Task Runtime | Task 生命周期用例、Claim/Recover/Cancel/Timeout/StartupCleanup 流程、事务编排、Repository、Command Receipt 实现 |
| Worker | Poll 循环、关闭宽限期内的等待行为、退避和 Runtime Host 协作 |
| Planner | PlannerRequest、候选解析、PlanDraft、ValidationIssue、一次 Repair 流程和 Prompt 构造 |
| Step Executor | StepExecutionRequest、输入值解析、动作分派、结果安全处理和模块专用 StepOutcome 载荷 |
| Tool Framework | Registry 实现、内部 capability 校验类型、Kubernetes Adapter DTO、ToolExecution Repository 和结果处理算法 |
| Approval | Approve/Reject 命令、Command Receipt、查询 DTO、审批事务和 Repository |
| Checkpoint | Save/Latest/RecoveryStart 请求、ValidationFacts、Codec、Repository、usage 校验矩阵和恢复来源校验流程 |
| Report | ReportFacts、Report 内容结构、查询 DTO、生成流程、Repository 和确定性安全文本构造 |

模块状态图、流程图和时序图仍由各模块维护，但其中引用的公共状态、动作、错误和跨模块 DTO 必须使用本文档定义。

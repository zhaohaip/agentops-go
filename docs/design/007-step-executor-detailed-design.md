# Step Executor 功能详细设计

| 属性 | 值 |
|---|---|
| 文档版本 | V1.17 |
| 文档状态 | MVP 详细设计 |
| 需求基线 | `docs/design/001-requirements.md` V3.5 |
| 架构基线 | `docs/design/003-system-architecture-design.md` V1.3 |
| 相邻详细设计 | `docs/design/004-task-runtime-detailed-design.md` V1.19、`docs/design/005-worker-detailed-design.md` V1.3、`docs/design/006-planner-detailed-design.md` V1.8、`docs/design/009-approval-detailed-design.md` V1.13、`docs/design/010-checkpoint-detailed-design.md` V1.8、`docs/design/008-tool-framework-detailed-design.md` V1.14 |
| 设计规则 | `docs/specs/005-detailed-design-guideline.md` |
| 共享契约 | `docs/design/002-shared-domain-contract.md` V1.1 |
| 契约修订 | P1-01：RequestApproval 统一为 Approval-owned Command/Result Port；P1-02：Tool Framework 公开 Port 固定为三个执行入口；P1-03：批准动作与Checkpoint证据分离 |

本文档中的 Step Executor 是应用层的单 Step 执行模块。它只由 Task Runtime 在已经领取当前 TaskExecution、加载并校验当前版本最新 Checkpoint 后调用。Step Executor 不领取 Task、不循环调度 Plan、不定义 Task 生命周期，也不直接生成最终 Report。

> 跨模块契约说明：ExecutionScope、StepOutcome、GenerationParams、Model Client、Tool Framework请求/结果、Approval Request、ApprovedAction/Evidence、next_action和公共错误语义以`docs/design/002-shared-domain-contract.md`为唯一规范来源。本文重复出现的字段或分支仅是Step Executor构造与映射说明。

> 类型约束：ExecutionScope、StepOutcome、ApprovedAction、ApprovedCheckpointEvidence及所有出站 DTO 的执行版本字段使用共享 `ExecutionVersion`；可空来源使用 `*ExecutionVersion`，Step Executor只原样传递。

## 1. 功能概述

### 1.1 功能目标

Step Executor 的目标是：

- 执行 Task Runtime 指定的当前顺序 Step；
- 按统一规则解析 `step.output.<field>` 紧邻前序输出引用；
- 对解析后的输入重新执行固定 Step 输入契约或 Tool Schema 校验；
- 将 ModelCall、Analysis 和 Verification 统一路由到 Model Client；
- 将 ToolCall 路由到 Tool Framework；
- 对低风险只读 Tool 自动执行；
- 对高风险 Deployment Patch 构造冻结审批现场并调用 Approval Manager；
- Approval 通过后只执行已冻结参数，不重新解析原始 Step 输入；
- 返回经过结构化、白名单、限长和脱敏处理的确定结果；
- 对写 Tool 无法确认外部结果的场景返回 UNKNOWN 副作用证据；
- 保留 execution_version、worker_id、取消、超时和迟到结果边界。

### 1.2 使用场景

Step Executor 覆盖：

1. 执行首次进入的 Pending ModelCall、Analysis、Verification 或 ToolCall Step；
2. 解析紧邻前一个 Completed Step 的直接输出字段；
3. 自动执行 Low 风险只读 Kubernetes Tool；
4. 为 High 风险 Deployment Patch 读取审批现场并进入 WaitingApproval；
5. Approve 后从当前版本自包含 Checkpoint 继续同一 ToolCall Step；
6. 执行前复核 Deployment 旧值和 resourceVersion；
7. 使用请求级 resourceVersion 原子前置条件执行受限 Patch；
8. ModelCall 或只读 Tool 中断后由 User Recover，在同一业务 Step 上重新执行；
9. Cancel、Task Timeout、动作超时、Runtime Shutdown 或 Lock Lost 时停止调用；
10. 外部结果返回后由 Task Runtime Guard 决定接收或丢弃。

### 1.3 涉及模块

| 模块 | 与 Step Executor 的关系 |
|---|---|
| Task Runtime | 唯一业务调用方；拥有执行编排、Active Call Registry、Step动作开始事务、结果事务和Task终态 |
| Step Executor Port | Task Runtime 依赖的最小单Step执行接口 |
| Shared Step Reference Extractor | Task Runtime、Checkpoint Manager与Step Executor共用的纯函数契约；只提取并规范化引用 |
| Input Resolver | 使用共享提取结果解析受限引用并执行运行期输入校验 |
| Model Client Port | 执行ModelCall、Analysis和Verification；隔离Eino和DeepSeek类型 |
| Tool Framework Port | 仅暴露三个类型化 Tool 入口；入口内部查找Tool、校验静态能力、创建ToolExecution调用边界并调用Kubernetes Adapter |
| Approval Request Port | 直接复用 Approval 模块的 RequestApprovalCommand 与 ApprovalRequestResult；Approval Manager独占等待事务 |
| Checkpoint Manager | Task Runtime在结果事务中调用；Step Executor只返回下一动作所需数据 |
| Runtime Write Executor | Tool Framework和Task Runtime通过持锁connection执行短事务；Step Executor不自行创建数据库连接 |
| Active Call Registry | Task Runtime拥有；向Step Executor传播同一可取消context |
| Task Lifecycle Policy | Task Runtime和Approval Manager使用；Step Executor不重复定义生命周期 |
| Worker | 不直接调用Step Executor，不解释StepOutcome |

Input Resolver、Step Dispatcher、Model Step Runner、Tool Step Runner和Result Processor均是Step Executor包内职责单元，不是独立服务、后台进程或持久化对象。

### 1.4 职责边界

Step Executor 负责：

- 校验 StepExecutionRequest 的应用契约；
- 识别当前执行动作是普通Step、请求审批或执行已批准Tool；
- 解析输入引用并生成不可变 ResolvedStepInput；
- 校验实际来源值和目标输入位置类型；
- 构造三类模型Step的固定请求；
- 调用一次Model Client并校验结构化输出；
- 调用Tool Framework执行只读Tool或已批准写Tool；
- 为未审批写Tool构造冻结ApprovalRequest；
- 对外部结果执行最终安全处理；
- 返回封闭的 StepOutcome 或系统 error。

Step Executor 不负责：

- 领取Task、Poll数据库或管理Worker执行槽；
- 选择下一个Task、Plan或Step；
- 创建、修改或重新规划Plan；
- 将全部历史Step、Memory或TaskLog注入模型；
- 持久化Step最终结果、Run Context或Checkpoint；
- 构造、保存或修改 Checkpoint 的 `resolved_references`；Task Runtime是唯一持久化构造Owner；
- 修改Task、Run或TaskExecution状态；
- 定义Cancel、Timeout、Recover或启动清理事务；
- 决定审批命令是否合法或提交审批决定；
- 生成Report；
- 自动重试Model或Tool；
- 自动重放UNKNOWN写Tool；
- 直接使用Eino、DeepSeek SDK或Kubernetes SDK类型。
- 推导或改写Checkpoint的`next_action`；该值由Task Runtime在持久化事务中按共享规则生成。

事务所有权固定为：

- Task Runtime：Step动作开始、Step确定结果、失败终态和Checkpoint事务；
- Approval Manager：进入WaitingApproval、Approve和Reject事务；
- Tool Framework：外部Tool调用前的ToolExecution=RUNNING边界事务；
- Step Executor：不拥有跨对象数据库事务，只编排上述端口并返回结果。

首个Step和后续Step的`next_action`必须使用第3.11节冻结的同一规则生成。Task Runtime的Planner结果事务生成首个执行Checkpoint，Task Runtime的Step结果事务生成后续执行Checkpoint，Approval Manager批准事务生成审批后Checkpoint，Task Runtime Recover事务生成Recovery Start Checkpoint。Worker、Step Executor和Checkpoint Manager均不得在运行期根据局部事实重新猜测或改写该值。

### 1.5 MVP 约束

- 单个不可变顺序Plan；
- 同一时刻只执行当前一个Step；
- 不支持DAG、并行Step、动态重规划或跳转；
- 不自动注入历史Step摘要；
- 只支持紧邻前序的`step.output.<field>`；
- ModelCall、Analysis和Verification共用Model Client；
- 模型固定为`deepseek-chat`，非流式调用；
- Model Step不执行结构Repair或自动重试；
- Tool由启动配置静态注册，运行期不变；
- 只读Tool自动执行；
- 写Tool仅支持受限Deployment Patch并要求一次Approval；
- 写Tool不提供exactly-once、自动重试、自动核验或自动回滚；
- 不新增StepExecution、ModelExecution、ModelAttempt或内存恢复事实。

## 2. 业务流程

### 2.1 普通 Step 执行

```mermaid
sequenceDiagram
    participant RT as Task Runtime
    participant SE as Step Executor
    participant IR as Input Resolver
    participant MC as Model Client
    participant TF as Tool Framework
    participant DB as PostgreSQL

    RT->>RT: 预登记PREPARED取消句柄
    RT->>DB: 动作开始短事务<br/>Guard + Pending→Running
    DB-->>RT: 提交成功
    RT->>RT: 句柄PREPARED→ACTIVE并检查context
    RT->>SE: ExecuteStep(context, request)
    SE->>IR: Resolve(step, previous_step)
    IR-->>SE: ResolvedStepInput
    alt ModelCall / Analysis / Verification
        SE->>MC: GenerateStructured(context, request)
        MC-->>SE: ModelResponse或ModelClientError
    else Low-risk read Tool
        SE->>TF: InvokeReadTool(context, ReadToolRequest<br/>scope含execution_config_hash)
        TF->>DB: ToolExecution=RUNNING短事务
        TF-->>SE: ToolFrameworkResult或system error
    end
    SE->>SE: OutputSchema校验与安全处理
    SE-->>RT: Completed / Failed / Stale
    RT->>DB: 结果短事务<br/>Step + ToolExecution + Run Context + Checkpoint
```

约束：

- 外部调用前后的数据库事务由持锁connection执行；
- Model和Kubernetes调用严格位于事务外；
- Step Executor使用Task Runtime传入的同一context；
- Completed只携带安全结构化结果；
- Task Runtime在保存前重新执行execution_version、worker_id、状态和deadline Guard。

### 2.2 请求 Approval

```mermaid
sequenceDiagram
    participant RT as Task Runtime
    participant SE as Step Executor
    participant TF as Tool Framework
    participant K8S as Kubernetes
    participant AM as Approval Manager
    participant DB as PostgreSQL

    RT->>RT: 预登记PREPARED取消句柄
    RT->>DB: 动作开始短事务<br/>Guard + Pending→Running
    DB-->>RT: 提交成功
    RT->>RT: 句柄PREPARED→ACTIVE并检查context
    RT->>SE: ExecuteStep(next_action=REQUEST_APPROVAL)
    SE->>SE: 解析引用并校验完整Patch输入
    SE->>TF: PrepareWriteApproval(context, PrepareWriteApprovalRequest<br/>scope含execution_config_hash)
    TF->>K8S: 读取Deployment当前字段和resourceVersion
    K8S-->>TF: ResourceSnapshot
    TF-->>SE: ApprovalPrepared(FrozenToolRequest)或其他封闭分支
    SE->>AM: RequestApproval(context, RequestApprovalCommand)
    AM->>DB: 单一短事务<br/>锁定执行事实
    AM->>AM: 校验Command结构及Scope/Frozen/Context/hash一致性
    alt 已存在完整相同WaitingApproval现场
        AM-->>SE: Existing
        SE-->>RT: WaitingApproval
    else 既有现场冲突或统一Guard失败
        AM-->>SE: Conflict / RuntimeFatal
        SE-->>RT: Stale / Failed/TaskTimeout / system error
    else Guard全部通过
        AM->>AM: 校验当前最大Checkpoint
        alt 可安全归属且CheckpointInvalid，Policy允许
            Note over AM,DB: Task Lifecycle Policy授权后<br/>Task/Run/Step/Execution=Failed/CheckpointInvalid<br/>清queued_at + Pending Report
            DB-->>AM: 提交成功
            AM-->>SE: CheckpointInvalid
            SE-->>RT: Terminalized
        else Checkpoint有效
            Note over AM,DB: Approval=Pending<br/>Step/Task/Run=WaitingApproval<br/>Execution=WAITING_APPROVAL<br/>Checkpoint + 清worker_id/queued_at
            DB-->>AM: 提交成功
            AM-->>SE: Pending
            SE-->>RT: WaitingApproval
        end
    end
```

在该流程中：

- 高风险Tool和其他Step使用相同的动作开始边界：`Pending → Running`必须先由Task Runtime提交；
- 输入解析失败发生在Step已经进入Running之后，由Task Runtime按普通Failed终态事务收敛；
- Kubernetes读取在Approval事务外；
- 不创建ToolExecution；
- 输入、旧值、新值和resourceVersion一经保存不可修改；
- Approval Manager负责完整等待事务，Step Executor和Task Runtime不得补写同一状态；
- WaitingApproval返回后Task Runtime结束当前执行循环并释放Worker。
- Approval Manager 只有在 current execution_version、worker_id、预期状态、deadline、DTO 等统一 Guard 全部通过，随后确认可安全归属的 CheckpointInvalid，并经共享 Task Lifecycle Policy 授权后，才能返回 `ApprovalRequestResult.CheckpointInvalid`；Step Executor只转换为 Terminalized，Task Runtime不得再次提交失败终态或重复创建 Report。

### 2.3 Approve 后执行 Patch

```mermaid
sequenceDiagram
    participant RT as Task Runtime
    participant SE as Step Executor
    participant TF as Tool Framework
    participant KA as Kubernetes Adapter
    participant API as Kubernetes API
    participant DB as PostgreSQL

    RT->>SE: ExecuteStep(next_action=EXECUTE_APPROVED_TOOL)
    SE->>SE: 校验当前Checkpoint直接引用的Approved Approval<br/>允许Recover已验证的跨版本不可变引用
    SE->>TF: InvokeApprovedWrite(context, ApprovedWriteRequest<br/>ApprovedAction + ApprovedCheckpointEvidence)
    TF->>API: 重新读取Deployment
    API-->>TF: 当前字段和resourceVersion
    alt 上下文已变化
        TF-->>SE: PreflightRejected(ApprovalContextChanged)
        SE-->>RT: Failed且无ToolExecution
    else 复核一致
        TF->>DB: ToolExecution=RUNNING<br/>冻结输入 + execution_version
        DB-->>TF: 提交成功
        TF->>TF: 调用前再次检查context
        TF->>KA: ExecuteApprovedPatch(context, frozen request)
        KA->>API: 单个JSON Patch请求<br/>test resourceVersion + 受限replace
        API-->>KA: 确定成功、确定拒绝或结果未知
        KA-->>TF: Adapter typed result
        TF-->>SE: ToolFrameworkResult或system error
        SE-->>RT: StepOutcome
        RT->>DB: 结果或终态短事务
    end
```

执行规则：

- Approved后不重新解析Step原始input；
- Worker和Step Executor不遍历历史execution_version的Checkpoint；
- 执行前读取只用于提前失败，请求级`resourceVersion` test才关闭读取与Patch之间的竞态；
- 原子test失败是确定的`ApprovalContextChanged`，不刷新Approval、不重试；
- 请求发出后在取得可信最终状态前超时、断连或结果不可确认时返回UNKNOWN并禁止重放。

### 2.4 取消、超时与迟到结果

```mermaid
sequenceDiagram
    participant RT as Task Runtime
    participant REG as Active Call Registry
    participant SE as Step Executor
    participant EXT as Model或Tool
    participant DB as PostgreSQL

    RT->>SE: ExecuteStep(context)
    SE->>EXT: 调用外部系统
    alt Cancel或Task Timeout
        RT->>DB: 先提交业务终态
        RT->>REG: cancel(TASK_CANCELLED或TASK_TIMED_OUT)
        REG-->>SE: context取消
        EXT-->>SE: 取消错误或迟到结果
        SE-->>RT: Stale或迟到结果
        RT->>DB: Guard失败，拒绝覆盖终态
    else ACTION_TIMEOUT
        REG-->>SE: cancel(ACTION_TIMEOUT)
        SE-->>RT: Failed/ModelCallFailed或ToolTimeout
        RT->>DB: Guard有效时提交当前Task失败终态
    else Runtime Shutdown或Lock Lost
        REG-->>SE: cancel(RUNTIME_SHUTDOWN或LOCK_LOST)
        SE-->>RT: Stale
        RT->>RT: 不再提交结果，交由下次StartupCleanup分类
    end
```

写Tool已经进入ToolExecution=RUNNING边界时，即使本地context成功取消也不能证明请求未到达Kubernetes；终止事务必须保留UNKNOWN和`side_effect_unknown=true`。

### 2.5 中断与 Recover

1. ModelCall中断时Step保持Running，当前TaskExecution进入INTERRUPTED；
2. Tool Step已经Running但当前版本尚无ToolExecution时仍位于副作用边界前，Step、Task和Run保持Running，TaskExecution进入INTERRUPTED；
3. 只读Tool中断时当前ToolExecution进入FAILED/WORKER_INTERRUPTED，Step保持Running，TaskExecution进入INTERRUPTED；
4. User Recover创建新的execution_version和自包含Recovery Start Checkpoint；
5. 新版本继续同一Running Step并重新执行Model或创建新的ToolExecution；
6. 已完成Step不重复执行；
7. 写Tool进入RUNNING后中断时TaskExecution、Step、Run和Task失败，ToolExecution进入UNKNOWN；
8. FAILED写Tool不允许Recover或重放。

若Recover发生在已批准写Tool尚未创建新版本ToolExecution之前，Recovery Start Checkpoint必须直接保存已经由Recover事务验证的旧版本Approved Approval引用及冻结输入；Approval记录不复制。新版本仍从副作用边界之前安全恢复。

## 3. 模块设计

### 3.1 模块定位

```mermaid
flowchart LR
    W["Task Worker"] --> RT["Task Runtime"]
    RT --> SEP["Step Executor Port"]
    SEP --> SE["Step Executor Application Service"]
    SE --> IR["Input Resolver"]
    SE --> MCP["Model Client Port"]
    SE --> TFP["Tool Framework Port"]
    SE --> AMP["Approval Request Port<br/>Approval-owned DTO"]
    MCP --> LLM["DeepSeek Adapter"]
    TFP --> K8S["Kubernetes Adapter"]
    AMP --> DB[("PostgreSQL")]
    TFP --> DB
    RT --> CM["Checkpoint Manager"]
    RT --> DB

    SE -. "禁止" .-> W
    SE -. "禁止直接访问" .-> DB
    SE -. "continuation由RT交付" .-> CM
    SE -. "禁止直接依赖" .-> LLM
    SE -. "禁止直接依赖" .-> K8S
```

依赖方向：

`Task Worker → Task Runtime → Step Executor Port → Step Executor Application Service → Model Client / Tool Framework / Approval Manager Ports`

Step Executor不会反向调用Task Runtime。Task Runtime在StepOutcome返回后调用Checkpoint Manager并提交结果事务，因此不形成`Task Runtime ↔ Step Executor`循环。

### 3.2 内部组成

| 组成 | 职责 |
|---|---|
| Request Validator | 校验ExecutionScope、Step快照、动作类型和关联对象 |
| Input Resolver | 识别、解析并类型校验受限引用 |
| Step Dispatcher | 按Step.type和Tool风险选择唯一执行分支 |
| Model Step Runner | 构造固定模型请求并执行一次Model调用 |
| Tool Step Runner | 编排只读Tool、Approval准备和已批准写Tool |
| Result Processor | 校验OutputSchema、执行限长与脱敏并生成StepOutcome |

上述组成共享一次调用内的不可变Request，不保存跨调用状态。

### 3.3 Step Executor Port

| 方法 | 调用方 | 输入 | 输出 |
|---|---|---|---|
| `ExecuteStep` | Task Runtime | context、StepExecutionRequest | StepOutcome或系统error |

接口约束：

- context必须来自Task Runtime当前Active Call Registry句柄；
- Request只包含应用值，不包含数据库模型、事务、Eino或Kubernetes SDK类型；
- Request.scope 必须是 Task Runtime 按共享契约第4节构造的完整 `ExecutionScope`；`execution_config_hash` 缺失或格式非法时返回 Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`，不解析输入、不调用 Model、Tool Framework 或 Approval Manager；
- 成功Outcome和error严格互斥；
- Outcome是封闭联合类型，调用方不得依赖错误字符串；
- Step Executor不得启动后台goroutine；
- 一次调用只表示一个逻辑Model或Tool动作；一个Kubernetes Tool动作允许按第8.1节固定上限发出必要的预检和变更请求；
- `WaitingApproval`表示Approval Manager已经提交完整等待现场；
- `Terminalized`表示Approval Manager已经提交Task级CheckpointInvalid终态和唯一Pending Report；
- `Completed`不表示结果已经持久化，Task Runtime仍须执行结果事务；
- 系统error沿Worker既有系统错误路径停止Runtime，不能转换成单Task失败。

### 3.4 StepExecutionRequest

| 字段 | 必填 | 含义 |
|---|---|---|
| `scope` | 是 | Task Runtime 构造的唯一共享 ExecutionScope，包含 task_id、run_id、execution_version、execution_config_hash、worker_id、step_id、deadline_at |
| `next_action` | 是 | EXECUTE_STEP、REQUEST_APPROVAL或EXECUTE_APPROVED_TOOL |
| `step` | 是 | 当前不可变Step执行投影 |
| `previous_step` | 按需 | sequence-1的Step状态、output和output_schema |
| `resolved_references` | 是 | 当前最大Checkpoint中已由Task Runtime构造、Checkpoint Manager验证的CanonicalResolvedReferences；无引用为空数组 |
| `agent` | 模型Step必填 | agent_id、system_prompt、model_name和不可变generation_params |
| `agent_authorization` | ToolCall必填 | agent_id和不可变allowed_tools投影 |
| `tool_capability` | ToolCall必填 | 当前静态Tool能力投影 |
| `approved_action` | 仅EXECUTE_APPROVED_TOOL必填 | 只由当前Checkpoint直接引用的不可变 Approved Approval 构造，不含Checkpoint/source字段 |
| `approved_checkpoint_evidence` | 仅EXECUTE_APPROVED_TOOL必填 | 只由当前版本最新有效Checkpoint构造的直接授权证据，不含完整冻结动作 |

当前Step执行投影包含：

- step_id、run_id、sequence、type、name；
- input、output_schema、tool_name；
- status；
- plan_id。

Request不得包含全部历史Step、TaskLog、Report、任意Memory、数据库Repository或当前持锁connection。

`resolved_references` 使用共享契约第7.4节冻结的 target_path 线协议、排序、去重、最大256条和最大深度16规则。StepExecutionRequest只接受EXECUTE_STEP、REQUEST_APPROVAL、EXECUTE_APPROVED_TOOL，因此固定使用TARGET_STEP_INPUT；GENERATE_PLAN/FINALIZE_RUN的NO_STEP_INPUT不调用Step Executor。Step Executor不能追加、删除、重排该列表，也不能把运行期解析结果返给 Task Runtime 作为下一 Checkpoint 的绑定。

`agent_authorization`由Task Runtime在三方 hash 门禁通过后，从计算当前`execution_config_hash`的同一不可变`ExecutionConfigV1.agent`投影，只包含`agent_id`和规范化后的`allowed_tools`集合；数据库不保存完整配置快照。Step Executor和Tool Framework不得在执行期间重新加载Agent配置或用当前新配置替换该投影。

### 3.5 ExecutionScope

> 唯一类型定义见共享契约第4节；本节只说明Step Executor交叉校验和原样传递。

ExecutionScope由《跨模块共享领域契约》第4节定义，Task Runtime 是唯一构造 Owner；Step Executor 只引用该类型，不声明本地副本：

| 字段 | 规则 |
|---|---|
| task_id、run_id、step_id | 非空且与请求投影一致 |
| execution_version | 必须为正且等于Task.current_execution_version的调用方已校验值 |
| execution_config_hash | 必填、非空且为64个小写十六进制字符；表示当前TaskExecution已通过门禁的配置版本 |
| worker_id | 必须为当前Runtime Instance标识 |
| deadline_at | Task Runtime从持久化Task事实读取的数据库截止时间；三个Tool请求原样携带 |

Step Executor 不计算、不补全、不查询配置刷新或修改 `execution_config_hash`。Scope不替代Task Runtime结果事务中的数据库Guard。

### 3.6 StepOutcome

> 封闭分支唯一来源见共享契约第2.2节；本节只冻结Step Executor载荷和Runtime映射。

| Outcome | 最小载荷 | 语义 |
|---|---|---|
| `Completed` | safe_output、可选tool_execution_id、tool_result_update、continuation | 已取得确定、安全结果，等待Task Runtime提交 |
| `WaitingApproval` | approval_id | Approval Manager已提交等待事务，Task Runtime立即结束执行循环 |
| `Terminalized` | task_id、execution_version、error_code=`CheckpointInvalid`、report_status=`Pending` | Approval Manager已提交Task级失败，Task Runtime只结束执行循环 |
| `Failed` | error_code、safe_summary、可选tool_execution_id、tool_result_update、side_effect_unknown | 当前动作确定失败，或写Tool结果未知且不可继续 |
| `Stale` | cause_code | 持久化事实或执行所有权已变化，不得推进状态 |

`continuation`只允许：

- `NEXT_STEP`加下一个step_id，由Task Runtime结果事务按第3.11节转换为持久化`next_action`；
- `FINALIZE_RUN`；
- WaitingApproval和Terminalized路径不返回continuation。

Completed和Failed中的Tool更新是供Task Runtime结果/终态事务使用的不可变草案，不是已提交事实。ToolExecution=RUNNING的创建结果已经由Tool Framework持久化。

只要本次调用已经创建ToolExecution，`Completed`或`Failed`就必须携带`tool_result_update`，将其收敛到COMPLETED、FAILED或UNKNOWN之一；禁止在后处理失败时遗漏更新并遗留RUNNING。

### 3.7 GenerationParams共享契约

> 唯一字段、类型、默认值和范围见共享契约第5.3节。

Planner、Step Executor和Model Client共用AgentOps自有的V1强类型`GenerationParams`，不得透传Eino、DeepSeek或OpenAI-Compatible的任意参数map：

| 字段 | 类型 | 规范化默认值 | 合法范围 |
|---|---|---:|---|
| `temperature` | `CanonicalDecimalV1` | 0.2 | 0到2，含边界 |
| `top_p` | `CanonicalDecimalV1` | 1 | 大于0且小于等于1 |
| `max_output_tokens` | `uint32` | 4096 | 1到8192，含边界 |

字段类型、范围和默认值属于共享Model Client契约；补默认值、拒绝未知/null/非有限值以及规范化序列化只由Task Runtime构造`ExecutionConfigV1.model`时执行。Step Executor只读取已经规范化的`generation_params`，不得维护字段顺序、默认JSON、hash输入或第二个序列化器。

Planner的INITIAL、REPAIR和本设计三类Model Step在同一TaskExecution中必须使用完全相同的不可变值。Model Client Adapter只负责把这三个字段映射到选定Eino版本的稳定模型选项；无法映射是启动配置错误，运行中发现投影矛盾则返回Runtime Fatal，不得静默忽略或使用Provider默认值。

### 3.8 Model Client Port

> 唯一Port和错误类别见共享契约第6节；本节只说明Model Step请求投影。

Step Executor复用AgentOps Model Client Port，不直接依赖Eino：

| 输入 | 规则 |
|---|---|
| model | 固定`deepseek-chat` |
| stream | 固定false |
| response_format | 固定`json_object` |
| messages | Agent system prompt、固定Step模板、ResolvedStepInput和OutputSchema |
| generation_params | 来自当前execution_config_hash对应的不可变静态Agent配置 |
| metadata | task_id、run_id、execution_version、step_id、step_type，仅用于进程内日志 |

ModelResponse只包含assistant content和允许的进程内元数据。第三方类型和provider_request_id不得进入Step、Checkpoint、TaskLog正文、Report正文或恢复上下文。

### 3.9 Tool Framework Port

> 三个公开方法、请求DTO及结果联合类型见共享契约第7.1节；本节只说明Step Executor构造和映射。

Step Executor直接依赖Tool Framework定义的共享Port契约，不在本模块复制DTO或结果类型：

| 导入方法 | 唯一请求 DTO | 统一结果 |
|---|---|---|
| `toolframework.ToolFrameworkPort.InvokeReadTool` | `toolframework.ReadToolRequest` | `toolframework.ToolFrameworkResult` 或独立 error |
| `toolframework.ToolFrameworkPort.PrepareWriteApproval` | `toolframework.PrepareWriteApprovalRequest` | `toolframework.ToolFrameworkResult` 或独立 error |
| `toolframework.ToolFrameworkPort.InvokeApprovedWrite` | `toolframework.ApprovedWriteRequest` | `toolframework.ToolFrameworkResult` 或独立 error |

```go
var toolFrameworkPort toolframework.ToolFrameworkPort
```

唯一 Go Interface 定义位于共享契约第7.1节；Tool Framework 第3.3节说明其实现，本节只规定 Step Executor 如何构造请求、调用并映射结果。

下文简写的 `ReadToolRequest`、`PrepareWriteApprovalRequest`、`ApprovedWriteRequest` 和 `ToolFrameworkResult` 均指 `toolframework` 包导出的同名类型，不是 Step Executor 本地类型。

三个请求的构造规则固定如下：

| 方法 | 唯一请求DTO | Step Executor构造来源 |
|---|---|---|
| `InvokeReadTool` | `ReadToolRequest` | `Scope=request.scope`（包含原 hash）、`agent_authorization`、`step.tool_name`、`ResolvedStepInput`、`tool_capability`对应的不可变StaticToolDefinition |
| `PrepareWriteApproval` | `PrepareWriteApprovalRequest` | `Scope=request.scope`（包含原 hash）、`agent_authorization`、`step.tool_name`、`ResolvedStepInput`、`tool_capability`对应的High/write StaticToolDefinition |
| `InvokeApprovedWrite` | `ApprovedWriteRequest` | `Scope=request.scope`（包含原 hash）、`agent_authorization`、原样 `approved_action`、原样 `approved_checkpoint_evidence`、`tool_capability`对应的High/write StaticToolDefinition |

禁止把上述DTO拆为位置参数，禁止使用`map[string]any`承载请求，也禁止在Step Executor中声明同结构的本地副本或兼容重载。三个 DTO 必须把入站 `StepExecutionRequest.scope` 整体按值复制；`deadline_at` 与 `execution_config_hash` 均不得重新查询、补算、替换或按入口分别构造。调用前若出站 DTO 的 Scope 与入站 Scope 任一字段不相等，返回 Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`，不调用 Tool Framework。

三个方法统一返回Tool Framework定义的封闭联合类型`ToolFrameworkResult`。允许分支及Step Executor映射固定如下：

| ToolFrameworkResult分支 | Step Executor处理 |
|---|---|
| `InvocationCompleted` | 生成`Completed`；若携带`processing_error`，生成`Failed`但保持`tool_result_update=COMPLETED`、output=NULL |
| `ApprovalPrepared` | 仅`PrepareWriteApproval`允许；取出其中`FrozenToolRequest`并调用Approval Manager |
| `PreflightRejected` | 生成无ToolExecution的`Failed`，保留稳定业务error_code |
| `ToolBusinessFailed` | 生成`Failed`；若分支携带已存在ToolExecution，则同时携带`tool_result_update=FAILED` |
| `SideEffectUnknown` | 生成`Failed`，携带`tool_result_update=UNKNOWN`及`side_effect_unknown=true` |
| `CheckpointInvalid` | 生成Task级`Failed/CheckpointInvalid`，交Task Runtime通过共享生命周期策略收敛 |
| `DeadlineExceeded` | 必须携带`cause_code=TaskTimeout`；生成`Failed/TaskTimeout`，不得自行提交终态 |
| `Stale` | 生成`Stale`，不得补写状态 |
| `RuntimeFatal` | 通过system error路径原样升级Task Runtime，停止当前执行循环 |
| 非nil `error` | 作为无法形成可信类型化结果的system error原样升级Task Runtime |

各方法允许分支固定为：

| 方法 | 允许分支 |
|---|---|
| `InvokeReadTool` | InvocationCompleted、ToolBusinessFailed、DeadlineExceeded、Stale、RuntimeFatal |
| `PrepareWriteApproval` | ApprovalPrepared、PreflightRejected、ToolBusinessFailed、DeadlineExceeded、Stale、RuntimeFatal |
| `InvokeApprovedWrite` | InvocationCompleted、PreflightRejected、ToolBusinessFailed、SideEffectUnknown、CheckpointInvalid、DeadlineExceeded、Stale、RuntimeFatal |

返回结果与`error`必须互斥。任一方法返回其允许集合之外的分支、空结果加nil error，或同时返回结果与error，Step Executor必须生成Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`，不得猜测业务含义。

Tool Framework负责：

- 再次校验Tool存在、enabled、allowed、risk_level、read_only和输入Schema；
- 只读/写权限、集群和namespace白名单；
- ToolExecution=RUNNING调用边界事务；
- Tool超时；
- Kubernetes Adapter调用；
- 原始Tool结果1 MiB限制、语义截断和初次脱敏；
- 返回共享封闭结果分支或独立system error。

`ValidateCapability` 不属于公开 Tool Framework Port。Step Executor、Planner 和 Approval 均不得调用或注入该函数，也不得引用 `ToolCapabilityRequest`、`ToolFrameworkError` 或 Tool Framework 内部校验结果。Step Executor 只依据当前持久化 Checkpoint 的 `next_action` 构造并调用上述三个公开请求之一；能力、静态定义和参数复核由被调用入口内部的纯函数完成，不能形成第四次跨模块调用。

`ApprovedAction` 与 `ApprovedCheckpointEvidence` 的唯一类型定义位于《跨模块共享领域契约》第7.2节。Step Executor 不声明本地副本：前者只包含 Approval 动作事实，后者只包含当前 Checkpoint 授权链事实。Step Executor 仅执行非空、格式和交叉绑定校验后原样放入 `ApprovedWriteRequest`，不得把 checkpoint/source 字段补入 Action，亦不得把完整冻结输入补入 Evidence。

实际业务请求中的Tool不存在、禁用或未授权时，Tool Framework分别返回`ToolBusinessFailed(ToolNotFound)`、`ToolBusinessFailed(ToolDisabled)`、`ToolBusinessFailed(ToolNotAuthorized)`。如果运行期静态Tool投影与计算同一execution_config_hash的不可变`ExecutionConfigV1.tool_framework.tools`投影不一致，返回`RuntimeFatal(RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT)`，不得伪装为当前Task Tool失败。

Tool Framework的起始事务返回`AlreadyStarted`时，必须转换为`ToolFrameworkResult.RuntimeFatal(PersistenceInvariantViolation)`：Step Executor不调用Kubernetes、不覆盖已有ToolExecution，并通过system error路径原样交给Task Runtime。数据库连接、事务提交结果不确定等无法构造可信结果的故障仍使用独立error通道。Runtime Host关闭当前实例后，由下一实例的StartupCleanup收敛已有记录。

测试用`FakeToolFrameworkPort`必须实现上述三个完全相同的方法签名，并遵守以下契约：

- 不提供 `ValidateCapability` 方法或能力预校验结果队列；
- 分别记录按调用顺序深拷贝后的`ReadToolRequest`、`PrepareWriteApprovalRequest`、`ApprovedWriteRequest`，同时记录context是否已取消；
- 深拷贝记录必须保留完整 ExecutionScope，尤其不得丢失或修改 `execution_config_hash`；
- 每个方法以FIFO方式返回预先配置的`(ToolFrameworkResult, error)`；
- 强制结果与error互斥，并拒绝当前方法不允许的结果分支；
- `PrepareWriteApproval`准备成功只能返回`ApprovalPrepared{FrozenToolRequest}`，Fake不得另设直接返回`FrozenToolRequest`的捷径；
- `RuntimeFatal`使用结果分支，数据库或事务提交不确定等system error使用error通道；
- 不调用真实Kubernetes、不启动goroutine、不修改请求DTO、不替Step Executor执行结果映射。

### 3.10 Approval Request Port 依赖

> 唯一RequestApproval签名、Command字段和Result分支见共享契约第7.3节。

《跨模块共享领域契约》第7.3节是该 Port、Command DTO 和 Result 的唯一契约源。Approval 模块实现该 Port；Step Executor 只导入并调用以下接口，不在本模块重新声明位置参数版本：

| 导入方法 | Command | Result |
|---|---|---|
| `approval.ApprovalRequestPort.RequestApproval` | `approval.RequestApprovalCommand` | `approval.ApprovalRequestResult` 或独立 error |

```go
var approvalRequestPort approval.ApprovalRequestPort
result, err := approvalRequestPort.RequestApproval(ctx, command)
```

Step Executor 构造 `approval.RequestApprovalCommand` 时：

- `Scope` 使用本次 `ExecuteStep` 收到的完整共享 `ExecutionScope`；
- `FrozenRequest` 使用 `PrepareWriteApproval` 成功返回的完整 `FrozenToolRequest`，不得拆分、复制字段清单或重新冻结；
- `StepID` 使用当前 Running Step；
- `ExecutionConfigHash` 原样复制 `Scope.execution_config_hash`；
- `ApprovalContext` 固定构造为 `NextAction=REQUEST_APPROVAL`、`ToolName=FrozenRequest.tool_name`、`RiskLevel=High`、`ReadOnly=false`；
- 构造前必须确认 `Scope.execution_config_hash` 与 `FrozenRequest.execution_config_hash` 均非空且相等；失败生成 Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`，不得调用 Approval Manager。

返回映射固定为：

| `ApprovalRequestResult` 分支 | Step Executor 结果 |
|---|---|
| `Pending` | `WaitingApproval` |
| `Existing` | `WaitingApproval`；不要求 Approval Manager 或 Task Runtime 重写等待现场 |
| `Conflict(cause_code=TaskTimeout)` | `Failed/TaskTimeout`，交 Task Runtime 按 Timeout 规则收敛 |
| 其他 `Conflict` | `Stale`；不得补写状态 |
| `CheckpointInvalid` | `Terminalized`；Approval Manager 已原子提交 Task 终态和 Pending Report |
| `RuntimeFatal` | 通过 system error 路径升级 Task Runtime |

结果与 `error` 必须互斥。未知分支、空结果加 nil error 或同时返回结果与 error 必须转换为 Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`。独立 `error` 原样升级 Task Runtime。

测试用 `FakeApprovalRequestPort` 必须：

- 实现与 Approval 模块完全相同的 `RequestApproval(ctx, command)` 方法签名；
- 按调用顺序深拷贝并记录完整 `approval.RequestApprovalCommand` 及 context 是否已取消；
- 不展开或维护独立 `FrozenToolRequest` 字段列表；
- 以 FIFO 返回预配置的 `(approval.ApprovalRequestResult, error)`；
- 只接受 `Pending`、`Existing`、`Conflict`、`CheckpointInvalid`、`RuntimeFatal` 五个结果分支，并强制结果与 error 互斥；
- 不访问数据库、不创建 Approval、不替 Approval Manager 执行 Guard 或事务。

Approval Manager 收到 Command 后重新锁定当前 Task、Run、Step 和 TaskExecution，按 Approval 设计执行唯一 Guard、幂等复用和 WaitingApproval 事务。Step Executor 不传入事务句柄，也不处理 Approve 或 Reject 命令。

传入Approval Manager的context必须是Task Runtime为当前动作预登记的同一个可取消context。context在等待事务提交前取消时不得提交WaitingApproval；事务已经提交后发生取消不能撤销已持久化的等待事实。

### 3.11 next_action共享生成规则

> 唯一枚举、生成规则与Owner见共享契约第2.1节。

`next_action`是Checkpoint持久化事实，不是Worker或Step Executor的运行期决策。对一个目标Step使用以下唯一纯函数：

| 目标Step及已持久化审批事实 | next_action |
|---|---|
| ModelCall、Analysis或Verification | `EXECUTE_STEP` |
| Low且read_only的ToolCall | `EXECUTE_STEP` |
| High且write的ToolCall，当前Checkpoint没有同一Tool动作的Approved Approval | `REQUEST_APPROVAL` |
| High且write的ToolCall，Approval Manager已批准同一冻结Tool动作，或Recover事务已验证并直接携带该Approved Approval | `EXECUTE_APPROVED_TOOL` |

写入责任固定为：

- Task Runtime的Planner结果事务：对Plan首个Step调用该函数并保存初始执行Checkpoint；
- Task Runtime结果事务：对`sequence+1` Step调用该函数并保存下一Checkpoint；
- Approval Manager Approve事务：保存直接引用Approved Approval的`EXECUTE_APPROVED_TOOL` Checkpoint；
- Task Runtime Recover事务：验证恢复来源后保存自包含Recovery Start Checkpoint，保持相应动作语义；
- Checkpoint Manager只校验和保存调用方给出的值，不推导业务动作；
- Worker和Step Executor只消费并校验当前版本最新Checkpoint中的值，不得降级为`EXECUTE_STEP`或自行跳过审批。

未支持的risk_level/read_only组合、Approval与目标Tool动作不一致或无法唯一生成动作时，事务不得创建Checkpoint。

## 4. 数据设计

### 4.1 持久化边界

Step Executor不新增数据库表或字段。

| 持久化对象 | 本模块产生的数据 | 实际写入者 |
|---|---|---|
| Step | safe output或稳定error_code | Task Runtime结果/终态事务 |
| ToolExecution | 调用输入、确定结果、失败或UNKNOWN草案 | RUNNING由Tool Framework；结果由Task Runtime |
| Approval | FrozenToolRequest | Approval Manager |
| Run Context | 当前Step结果引用和下一位置 | Task Runtime |
| Checkpoint | continuation对应的Runtime Context | Task Runtime调用Checkpoint Manager |
| TaskLog | 最小安全执行事件 | 第7.6节为每个事件指定的唯一Owner |

原始Model/Tool响应、未解析候选、完整Prompt、凭证和SDK错误不得持久化。

### 4.2 ResolvedStepInput

ResolvedStepInput是内存值：

| 字段 | 含义 |
|---|---|
| `step_id` | 当前Step |
| `value` | 引用替换后的完整JSON object |
| `referenced_fields` | 与请求canonical resolved_references完全相同的只读规范绑定 |
| `input_contract_version` | 与Planner及Step Executor共享的固定契约版本 |

ResolvedStepInput不保存到新表。Task Runtime已经在持久化Checkpoint时构造绑定；Step Executor只复核规范绑定并替换值，不是Checkpoint输入的反向生产者。

### 4.3 ToolFrameworkResult共享投影

`ToolFrameworkResult`及其全部分支只在Tool Framework Port契约中定义。Step Executor只做穷尽匹配，不维护第二份结果DTO。共享分支为：

- `InvocationCompleted`；
- `ApprovalPrepared`；
- `PreflightRejected`；
- `ToolBusinessFailed`；
- `SideEffectUnknown`；
- `CheckpointInvalid`；
- `DeadlineExceeded(cause_code=TaskTimeout)`；
- `Stale`；
- `RuntimeFatal`。

三个方法各自允许的分支集合以共享契约第7.1节为唯一契约。只读Tool不得返回`SideEffectUnknown`；写Tool只有进入ToolExecution=RUNNING边界后才允许返回该分支。`DeadlineExceeded`固定携带`cause_code=TaskTimeout`，返回后由Task Runtime使用既有Timeout终态事务收敛，不由Tool Framework修改Task状态。

写Tool结果还必须保留Kubernetes Adapter冻结的最终状态事实：未取得可信最终HTTP/Kubernetes状态时只能返回`SideEffectUnknown`；明确2xx只能返回`InvocationCompleted`；明确非2xx只能返回`ToolBusinessFailed`。明确2xx后的body读取、解析、OutputSchema、脱敏或大小失败通过`InvocationCompleted.processing_error`表达，output=NULL，不得由Tool Framework或Step Executor回退`SideEffectUnknown`；明确非2xx的错误body无法解析也不得回退`SideEffectUnknown`。Cancel/Timeout已先提交或结果持久化失败仍按Task Runtime既有竞态规则处理。

外部调用事实与Step后处理结果分开表达：

| 外部调用事实 | OutputSchema/安全/大小后处理 | ToolExecution最终状态 | Step最终结果 |
|---|---|---|---|
| 确定成功 | 成功 | COMPLETED，保存safe output | Completed |
| 确定成功 | OutputSchema失败 | COMPLETED，output不保存原始值 | Failed/StepOutputInvalid |
| 确定成功 | 脱敏或安全处理失败 | COMPLETED，output不保存原始值 | Failed/ResultSanitizationFailed |
| 确定成功 | safe output超过上限 | COMPLETED，output不保存原始值 | Failed/StepOutputTooLarge |
| 确定失败 | 不适用 | FAILED | Failed/对应Tool错误 |
| 写Tool结果无法确认 | 不适用 | UNKNOWN且side_effect_unknown=true | Failed/对应未知结果错误 |

Tool已经确定成功但Step后处理失败时，`Failed.tool_result_update`必须把ToolExecution更新为COMPLETED；该状态只陈述外部调用事实，不表示Step成功。ToolExecution仅保存调用标识、时间、确定成功状态和允许的安全元数据，`output`保持NULL，不保存无法通过校验或安全处理的内容。若结果事务本身无法可靠提交，仍按`PersistenceAfterWriteFailed`或StartupCleanup规则处理，不把“已知外部成功”误当作后处理成功。

ToolExecution不持久化`read_only`。Step Executor对Tool读写属性的判断只使用请求中的`tool_capability`，该投影必须来自 Task Runtime 计算当前 TaskExecution.execution_config_hash 的同一不可变`ExecutionConfigV1.tool_framework.tools`；ToolExecution的status、error_code或side_effect_unknown不能反向推导或覆盖该定义。

### 4.4 ApprovedAction 与 ApprovedCheckpointEvidence

> 唯一字段集合和来源边界见共享契约第7.2节；execution_version标量冲突见共享契约第10.1节。

采用方案 B，并直接引用共享契约第7.2节的唯一共享定义。Task Runtime 在加载并验证当前版本最新 Checkpoint、其直接引用的不可变 Approval 和当前 TaskExecution 后构造两个互斥投影：

| DTO | 唯一事实来源 | 不得包含 |
|---|---|---|
| `ApprovedAction` | Approval | checkpoint_id、checkpoint_type、当前Checkpoint版本、source_execution_version、source_checkpoint_id |
| `ApprovedCheckpointEvidence` | 当前版本最新有效 Checkpoint | 完整冻结输入、observed_values、resourceVersion、Task/Run/Step/Tool归属、Approval状态、worker_id |

`StepExecutionRequest` 必须同时携带二者。Step Executor 不从 Checkpoint 重建 ApprovedAction，也不从 Approval 推导 Evidence，只验证：

- `ApprovedAction.approval_status=Approved`；
- `ApprovedAction.approval_id=Evidence.approval_id`；
- `ApprovedAction.frozen_input_hash=Evidence.frozen_input_hash`；
- `ApprovedAction.execution_config_hash=Evidence.execution_config_hash=scope.execution_config_hash`；
- Action 的 task/run/step 与 Scope、当前 Step 相同；
- 同版本时 Evidence 为 `APPROVED_CONTINUATION`，source 两字段为空，Action Approval 版本、Evidence版本与Scope版本相等；
- Recover 时 Evidence 为 `RECOVERY_START`，source 两字段同时存在，Evidence版本等于Scope版本且source版本等于Scope版本减一；连续Recover允许Action的Approval原始版本小于source版本。

`frozen_input_hash` 是对 `FrozenApprovedToolInputV1{tool_name,tool_input,observed_values,resource_version}` 规范 JSON 的 SHA-256 小写十六进制摘要，用于在不复制完整冻结动作的情况下绑定两个 DTO。Step Executor 不计算或补写该 hash。

当前最大 Checkpoint 缺失、结构无效或 Recovery source 不完整按 Task 级 `CheckpointInvalid`；已经声明验证通过的两个 DTO 仍在 approval_id、frozen_input_hash、execution_config_hash、版本模式或归属上矛盾，按 Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`。不得调用 Tool Framework 后再猜测或修复证据。

Recover 后的当前 Recovery Start Checkpoint必须自包含并直接保存来源版本、来源Checkpoint、同一approval_id和相同frozen_input_hash。不得复制Approval，不得在Worker或Step Executor中递归遍历历史Checkpoint。

### 4.5 OutputSchema运行期契约

Step Executor复用共享契约第7.5节的AgentOps OutputSchema：

- 顶层是1到32个直接字段的object；
- 每个字段只声明`type`；
- 类型只允许string、number、integer、boolean、object、array；
- 字段名区分大小写；
- null不满足任何声明类型；
- object和array只作为完整直接字段，不解析多级引用。

Model Step输出必须是严格单个JSON object：

- 所有声明字段必须存在；
- 不允许未声明字段；
- 值类型必须匹配，integer可用于number；
- 任意层级重复JSON key、非有限number、尾随文本和Markdown fence均非法。

Tool结果使用声明字段投影：

- 每个声明字段必须存在且类型匹配；
- 只将声明字段复制到Step.output；
- Tool Framework内部返回的其他安全元数据不进入Step.output；
- ToolExecution可单独保存需求允许的truncated、original_size和original_count。

### 4.6 固定输入契约

ModelCall：

| 字段 | 必填 | 类型 | 引用 |
|---|---|---|---|
| prompt | 是 | 非空string | 允许string引用 |
| context | 否 | object | 允许object引用 |

Analysis：

| 字段 | 必填 | 类型 | 引用 |
|---|---|---|---|
| instruction | 是 | 非空string | 允许string引用 |
| evidence | 是 | object | 允许object引用 |

Verification：

| 字段 | 必填 | 类型 | 引用 |
|---|---|---|---|
| criteria | 是 | 非空string | 禁止引用 |
| evidence | 是 | object | 允许object引用 |

三类input禁止附加顶层字段和null。ToolCall复用共享契约第7.5节冻结的Tool Schema子集；Step Executor不得扩展`$ref`、组合Schema、多类型联合或动态additionalProperties。

### 4.7 资源限制

| 限制 | 固定值或来源 | 处理 |
|---|---:|---|
| 引用和输入JSON最大深度 | 16 | InputResolutionFailed或STEP_EXECUTOR_CONTRACT_BROKEN |
| 单Step规范引用数量 | 256 | 运行中超限为STEP_EXECUTOR_CONTRACT_BROKEN/REFERENCE_COUNT_LIMIT_EXCEEDED |
| 单个object最大字段数 | 64 | 同上 |
| 引用替换后的ResolvedStepInput | 1 MiB | InputResolutionFailed |
| OutputSchema字段数 | 32 | 运行中出现超限视为STEP_EXECUTOR_CONTRACT_BROKEN |
| Model原始响应 | 1 MiB | ModelCallFailed/MODEL_RESPONSE_TOO_LARGE |
| Tool原始结果 | 1 MiB | 由Tool Framework安全语义截断 |
| 可持久化Step.output | 1 MiB | StepOutputTooLarge |
| Model Step总Prompt | 256 KiB | ModelInputTooLarge，不调用模型 |
| 单次Model Step调用 | 60 seconds | min(60s，上层context剩余时间) |
| 单次Tool调用 | Tool静态配置timeout | min(Tool timeout，上层context剩余时间) |
| Container Log默认/最大行数 | 200 / 1000 | 超过请求上限拒绝 |
| `safe_summary` | 512 UTF-8 bytes | 使用固定安全模板并按语义截断 |
| 单个结构化日志string字段 | 256 UTF-8 bytes | 按语义截断 |

所有大小按UTF-8字节计算，并逐字段对应共享`ExecutionConfigV1.json`、`safety`、`step_executor.limits`和`tool_framework.result_limits`。Step Executor不得通过“Step契约版本”隐式追加局部hash输入；网络连接、DNS和TLS等排除项以共享契约第5.4节为准。

字节计算使用确定性JSON：UTF-8、object key按字典序、无非必要空白、string使用标准JSON转义、number使用最短无损十进制表示、integer不得带小数点。根JSON值深度为1，每进入一个object或array子值增加1。该规则同时用于ResolvedStepInput大小、冻结输入相等性、Step.output大小和测试边界。

`safe_summary`只允许由稳定error_code、经过白名单的对象标识和固定模板生成，禁止拼接Provider/Kubernetes原始错误、输入值或响应内容。超过512字节时优先裁剪可选安全标识；仍超限时在合法UTF-8字符边界截断并把末尾替换为单字符`…`，且省略号计入上限。结构化日志string字段采用同一规则和256字节上限。

### 4.8 数据不变量

- 当前Step等于Run.current_step_id；
- 当前Step属于Run唯一Plan；
- previous_step只能是sequence-1；
- 引用来源Step必须Completed；
- 引用值来自已经安全持久化的Step.output；
- EXECUTE_APPROVED_TOOL只使用当前Checkpoint直接引用的Approved Approval；
- 当前Checkpoint和新建ToolExecution必须匹配当前execution_version；
- Approval自身可以属于旧execution_version，但仅允许由当前Recovery Start Checkpoint按第4.4节直接、完整地引用；
- 非Recovery Start Checkpoint引用的Approval必须与当前execution_version一致；
- Step.output只包含OutputSchema声明字段；
- ToolExecution输入是实际调用的规范化输入；
- 已批准写Tool的调用输入与Approval冻结输入完全一致；
- UNKNOWN只能用于写Tool且必须`side_effect_unknown=true`；
- Completed/Failed确定结果不得被后续迟到结果覆盖。

## 5. 状态设计

### 5.1 Step持久化状态

> StepStatus唯一枚举和终态见共享契约第1.3节；本状态图只展示本模块相关路径。

```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Running: Task Runtime动作开始事务
    Running --> WaitingApproval: Approval Manager等待事务
    WaitingApproval --> Running: Approval Manager Approve事务
    WaitingApproval --> Failed: Reject、Cancel或Timeout
    Running --> Completed: Task Runtime结果事务
    Running --> Failed: Task Runtime失败终态事务
    Completed --> [*]
    Failed --> [*]
```

Step Executor不直接提交上述转换。恢复Model/只读Tool时Step保持Running，不执行`Running→Pending`。

### 5.2 Step Executor调用状态

```mermaid
stateDiagram-v2
    [*] --> Validating
    Validating --> ResolvingInput
    Validating --> SystemError: 内部契约破坏
    ResolvingInput --> Failed: InputResolutionFailed
    ResolvingInput --> Dispatching
    Dispatching --> CallingModel
    Dispatching --> CallingReadTool
    Dispatching --> PreparingApproval
    Dispatching --> CallingApprovedTool
    CallingModel --> ProcessingResult
    CallingReadTool --> ProcessingResult
    CallingApprovedTool --> ProcessingResult
    PreparingApproval --> WaitingApproval
    PreparingApproval --> Terminalized: Approval Manager已提交CheckpointInvalid终态
    ProcessingResult --> Completed
    ProcessingResult --> Failed
    CallingModel --> Stale: 业务终态或Runtime停止
    CallingReadTool --> Stale: 业务终态或Runtime停止
    CallingApprovedTool --> Stale: 业务终态或Runtime停止
    Completed --> [*]
    Failed --> [*]
    WaitingApproval --> [*]
    Terminalized --> [*]
    Stale --> [*]
    SystemError --> [*]
```

该状态仅存在于一次函数调用内，不持久化。

### 5.3 ToolExecution状态

> ToolExecutionStatus唯一枚举与UNKNOWN语义见共享契约第1.5节。

```mermaid
stateDiagram-v2
    [*] --> RUNNING: Tool Framework开始事务
    RUNNING --> COMPLETED: 确定成功
    RUNNING --> FAILED: 确定失败
    RUNNING --> UNKNOWN: 写Tool结果无法确认
    COMPLETED --> [*]
    FAILED --> [*]
    UNKNOWN --> [*]
```

- 只读Tool只能进入COMPLETED或FAILED；
- Approval等待和Reject不创建ToolExecution；
- 请求级resourceVersion test冲突是FAILED/ApprovalContextChanged；
- 写Tool在取得可信最终状态前超时、断连、Worker中断或无法确认结果进入UNKNOWN；
- UNKNOWN不可恢复、不可重放。

### 5.4 Outcome与持久化状态关系

| Outcome | 已提交事实 | Task Runtime后续行为 |
|---|---|---|
| Completed | Tool调用时ToolExecution已为RUNNING；Model无调用记录 | 原子提交Step Completed、ToolExecution Completed、Context和Checkpoint |
| WaitingApproval | Approval等待现场已完整提交 | 不再写状态，返回Worker |
| Failed | 可能有RUNNING ToolExecution；若已创建则Outcome必含终态更新 | 按error_code和副作用分类原子收敛ToolExecution并提交Task失败终态 |
| Stale | 持久化事实已由并发命令改变，或Runtime停止 | 丢弃结果，不推进状态 |
| 系统error | 结果无法安全归属或内部不变量破坏 | Worker停止新Claim，Runtime Host关闭 |

## 6. 核心逻辑

### 6.1 启动不变量与请求校验

Runtime启动时必须先验证：

- Model配置固定为DeepSeek `deepseek-chat`且可构造Model Client；
- Agent引用的Tool存在、enabled且Schema属于共享契约第7.5节冻结子集；
- Tool的risk_level与read_only组合只能是Low/read_only或High/write；
- 所有High/write Tool均为本MVP受限Deployment Patch；
- Tool timeout为正；
- Kubernetes集群、namespace、replicas范围和image Registry白名单配置结构合法；
- 固定Step输入契约、OutputSchema和资源限制版本一致。
- GenerationParams已补齐默认值、满足第3.7节类型和范围，并能被Model Client稳定映射；

失败时整个Runtime拒绝启动，不把静态配置错误延迟为单Task失败。

按以下顺序校验：

1. context、scope和step存在；
2. ID、execution_version、worker_id合法，scope.execution_config_hash非空且为64个小写十六进制字符；
3. step.run_id、step_id与scope一致；
4. step.status是Pending或Running；已批准继续必须为Running；
5. next_action与Step.type、status和approved_action组合一致；
6. Model Step具有同一静态Agent配置；
7. ToolCall具有tool_name、AgentAuthorization和静态Tool能力，且tool_name存在于allowed_tools；
8. previous_step存在性与引用需求一致；
9. 使用共享提取器从step.input重新计算的CanonicalResolvedReferences与请求`resolved_references`数量、顺序及各字段完全相同；
10. output_schema满足共享协议。

第2～10项若是调用方交付的DTO与已经完成的持久化校验结果互相矛盾，属于`STEP_EXECUTOR_CONTRACT_BROKEN`系统错误；若Task Runtime在调用前发现当前最大Checkpoint缺失、无法解析、resolved_references与持久化Step.input不完全一致或其他关联非法，则不得调用Step Executor，并按`CheckpointInvalid`终止当前Task。合法的版本、状态或所有权竞争返回Stale。

### 6.2 引用识别

Step Executor按当前三种next_action固定使用TARGET_STEP_INPUT，调用共享契约第7.4节同一`StepReferenceExtractor`递归遍历input的JSON叶子：

- 完整匹配`^step\.output\.[A-Za-z_][A-Za-z0-9_]*$`的string是引用；
- 以保留前缀`step.output.`开头但不完整匹配的string是非法引用；
- 包含`${...}`、`{{...}}`、多级路径、数组下标、函数、条件或默认值语法的值非法；
- 不以保留前缀开头、只在字符串中部包含`step.output.`的普通文本按字面量处理；
- MVP不提供转义语法；需要以保留前缀开头的字面量不属于支持范围。

引用只能独占一个JSON值。提取器同时生成规范target_path，执行最大深度、最大数量、重复target和确定性排序规则。Step Executor不维护私有的第二套遍历或排序实现。

### 6.3 引用解析与运行期校验

先要求共享提取器结果与请求`resolved_references`完全一致且数量不超过256；不一致或超限表示上游宣称已通过Planner/Checkpoint校验却交付矛盾DTO，返回`STEP_EXECUTOR_CONTRACT_BROKEN`，稳定cause_code分别使用既有引用差异reason或`REFERENCE_COUNT_LIMIT_EXCEEDED`，且不开始外部动作。然后对每个规范引用：

1. 当前Step.sequence必须大于1；
2. previous_step.sequence必须等于当前sequence-1；
3. previous_step.status必须为Completed；
4. 字段必须存在于previous_step.output_schema；
5. previous_step.output必须是可解析object；
6. 实际字段必须存在；
7. 实际值禁止null；
8. 实际值类型必须满足来源OutputSchema；
9. 来源声明类型必须满足当前目标位置类型；
10. 把实际值深拷贝到目标位置；
11. 替换后完整input重新执行固定输入契约或Tool Schema校验。
12. 规范化后的ResolvedStepInput不得超过1 MiB。

任一失败返回`Failed/InputResolutionFailed`，不创建ToolExecution、不调用Model、Tool或Approval Manager。

### 6.4 Step分派

分派矩阵固定为：

| Step.type | 条件 | 路径 |
|---|---|---|
| ModelCall | EXECUTE_STEP | Model Step Runner |
| Analysis | EXECUTE_STEP | Model Step Runner |
| Verification | EXECUTE_STEP | Model Step Runner |
| ToolCall | Low + read_only + EXECUTE_STEP | InvokeReadTool |
| ToolCall | High + !read_only + REQUEST_APPROVAL | PrepareWriteApproval |
| ToolCall | High + !read_only + EXECUTE_APPROVED_TOOL | InvokeApprovedWrite |

其他组合是系统契约错误。MVP不支持Low写Tool、High只读Tool或非Deployment写Tool。

该矩阵只校验持久化`next_action`是否与请求事实一致，不负责生成`next_action`；生成规则唯一来源是第3.11节。

### 6.5 Model Step执行

Model请求按固定区块构造：

1. Agent system prompt；
2. 平台安全边界；
3. Step类型模板；
4. 当前Step name；
5. ResolvedStepInput；
6. AgentOps OutputSchema及严格JSON object要求。

类型模板：

- ModelCall：执行`prompt`，仅使用可选`context`；
- Analysis：按`instruction`分析`evidence`；
- Verification：仅按`criteria`核对`evidence`并输出声明字段。

禁止：

- 注入Task全部原始输入，除非它已经作为当前Step显式input；
- 注入非紧邻历史Step、TaskLog、Checkpoint、Approval评论或Memory；
- 让模型新增、删除或重排Step；
- 把Verification描述成Deployment rollout或应用健康恢复，除非evidence和criteria明确提供对应事实；Patch后的MVP Verification只确认批准目标字段一致。

调用规则：

1. Prompt超过256 KiB返回`Failed/ModelInputTooLarge`；
2. 单次timeout为`min(60s, 上层context剩余时间)`；
3. 使用同一context调用Model Client；
4. stream=false、response_format=json_object；
5. 不自动重试、不Repair、不Fallback；
6. Provider、认证、限流、网络或timeout返回`Failed/ModelCallFailed`及安全cause_code；
7. ACTION_TIMEOUT在Guard仍有效时返回`Failed/ModelCallFailed`，由Task Runtime终止当前Task；
8. TASK_CANCELLED或TASK_TIMED_OUT已经有持久化终态，返回Stale；
9. RUNTIME_SHUTDOWN或LOCK_LOST返回Stale，旧实例不得写结果。

### 6.6 Model输出处理

1. 在Model Client读取阶段执行1 MiB硬限制；
2. 只接受一个JSON object；
3. 拒绝重复key、未知字段、缺失字段、null、非有限number和尾随文本；
4. 按OutputSchema检查全部直接字段类型；
5. 执行安全处理；
6. 规范化后结果不得超过1 MiB；
7. 成功返回Completed；
8. 任一解析、Schema或安全错误返回`Failed/ModelOutputInvalid`。

Model Step没有一次Repair能力；Planner的一次Repair策略不适用于执行期Step。

### 6.7 只读Tool执行

1. Step Executor从当前`StepExecutionRequest`构造唯一`ReadToolRequest`，完整原样复制含 execution_config_hash 的 scope，不得拆散scope、授权、输入或Tool Definition；
2. 调用`InvokeReadTool(context, request)`；Tool Framework使用DTO中的AgentAuthorization再次校验Tool存在、enabled、allowed、Low、read_only；
3. Tool Framework校验解析后完整input和集群/namespace白名单；
4. 在Tool调用开始事务中重新Guard scope、TaskExecution=RUNNING、Step=Running和deadline；
5. 数据库UTC已到deadline时不创建ToolExecution，返回`DeadlineExceeded(cause_code=TaskTimeout)`；
6. 未到期时创建当前execution_version的ToolExecution=RUNNING；
7. 事务提交后检查同一context，并在事务外调用Kubernetes；
8. 结果超过1 MiB时按Tool语义截断，设置truncated和可确定的original_size/original_count；
9. Tool Framework对结果执行初次脱敏，Step Executor按output_schema投影并执行最终安全检查；
10. Step Executor按第3.9节穷尽处理`ToolFrameworkResult`；只读入口只允许`InvocationCompleted`、`ToolBusinessFailed`、`DeadlineExceeded`、`Stale`或`RuntimeFatal`。

只读Tooltimeout或连接失败返回`ToolBusinessFailed`并携带FAILED ToolExecution草案，使用`ToolTimeout`或`ToolConnectionLost`；不得使用`SideEffectUnknown`。
Tool Framework不得为校验allowed_tools重新加载Agent配置。

### 6.8 未审批写Tool

1. 完整解析引用；
2. 使用请求携带的AgentAuthorization、Tool Schema、字段白名单、replicas范围、image Registry白名单和集群/namespace权限校验；
3. 禁止任意JSON Patch、Merge Patch或完整Deployment对象；
4. 构造唯一`PrepareWriteApprovalRequest`，完整原样复制含 execution_config_hash 的 scope，并调用`PrepareWriteApproval(context, request)`；
5. 仅`ApprovalPrepared`表示准备成功，从该分支取出`FrozenToolRequest`；不得把Port调用声明为直接返回`FrozenToolRequest`；
6. `PreflightRejected`或`ToolBusinessFailed`返回对应`Failed`；`DeadlineExceeded(cause_code=TaskTimeout)`返回`Failed/TaskTimeout`；`Stale`不补写状态；`RuntimeFatal`或system error原样升级；此入口出现其他分支按`STEP_EXECUTOR_CONTRACT_BROKEN`处理；
7. 准备成功后构造 Approval 模块定义的唯一 `RequestApprovalCommand`：原样设置 `Scope`、完整 `FrozenRequest`、当前 `StepID` 和 `ExecutionConfigHash`，并设置固定 `ApprovalContext`；不得使用位置参数或本地 DTO；
8. 调用 `RequestApproval(context, command)` 并穷尽映射 `ApprovalRequestResult`：`Pending`、`Existing`→`WaitingApproval`；`CheckpointInvalid`→`Terminalized`；`Conflict(cause_code=TaskTimeout)`→`Failed/TaskTimeout`；其他 `Conflict`→`Stale`；`RuntimeFatal` 或 system error 原样升级；
9. 返回未知分支、结果/error不互斥或 Command 构造后 hash 被改变时，返回 Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`，不得调用或重试 Approval Manager。

参数非法时不读取Deployment、不创建Approval或ToolExecution，返回`Failed/ToolInputInvalid`。读取现场的确定失败返回对应Tool错误；context因已提交终态取消时返回Stale。

### 6.9 已批准写Tool

1. 当前next_action必须为EXECUTE_APPROVED_TOOL；
2. approved_action必须只来自当前版本最大Checkpoint直接引用的持久化 Approval；approved_checkpoint_evidence必须只来自该当前最大Checkpoint；
3. 按第4.4节校验Approved、approval_id、frozen_input_hash、execution_config_hash、归属和同版本/Recovery版本矩阵；
4. 使用Approval.tool_input，不重新解析Step.input；
5. 对冻结输入执行完整性和静态安全配置校验，并构造唯一`ApprovedWriteRequest`，其中原样放入scope、authorization、approved_action、approved_checkpoint_evidence和静态Tool Definition；不得跨DTO复制字段；
6. 调用`InvokeApprovedWrite(context, request)`；Tool Framework在事务外读取Deployment并比较resourceVersion和待修改字段；
7. 已变化时返回`PreflightRejected/ApprovalContextChanged`，Step Executor生成无ToolExecution的`Failed`；
8. Tool开始事务使用数据库UTC再次检查deadline；已到期时不创建ToolExecution并返回`DeadlineExceeded(cause_code=TaskTimeout)`；
9. 未到期时创建ToolExecution=RUNNING并冻结实际请求；
10. 提交后检查context；尚未取得最终状态时，写Tool在RUNNING边界后的取消按`SideEffectUnknown`处理；已经取得明确2xx或非2xx时保留该最终状态事实；
11. Adapter只根据冻结结构化输入生成JSON Patch，User和模型不得提供Patch path、operation或容器数组下标；
12. 同一请求先执行`test /metadata/resourceVersion`，再按需生成`replace /spec/replicas`，或由Adapter按冻结container_name定位索引后生成对应image replace；
13. 请求级test冲突返回确定`ToolBusinessFailed/ApprovalContextChanged`；
14. 明确API拒绝且能确认未生效时返回`ToolBusinessFailed`；
15. 最终状态取得前超时、断连、响应丢失或其他无法确认结果返回`SideEffectUnknown`；明确2xx后的body处理失败返回`InvocationCompleted`及processing_error，明确非2xx后的错误body处理失败返回`ToolBusinessFailed`；
16. `CheckpointInvalid`按Task级失败收敛；`Stale`不补写；`RuntimeFatal`或system error原样升级；此入口出现其他不允许分支按`STEP_EXECUTOR_CONTRACT_BROKEN`处理；
17. 不刷新resourceVersion、不复用Approval、不重试、不回滚。

当前最大Checkpoint缺失、无法解析、关联非法或Recovery来源证据不完整属于Task级`CheckpointInvalid`，应由Task Runtime在调用前终止；只有调用方在宣称该校验已经通过后仍传入互相矛盾的DTO，才属于Runtime Fatal `STEP_EXECUTOR_CONTRACT_BROKEN`。静态配置投影自相矛盾属于Runtime Fatal `RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT`。Kubernetes当前资源相对已批准现场发生变化属于当前Task的`ApprovalContextChanged`。

### 6.10 结果安全处理

所有Model和Tool结果在返回Completed前：

- 只保留OutputSchema声明字段；
- 递归检查object key；按ASCII小写后精确匹配`password`、`passwd`、`secret`、`token`、`api_key`、`apikey`、`private_key`、`client_secret`或`authorization`时，把对应值整体替换为`[REDACTED]`；
- string在任意行出现`-----BEGIN PRIVATE KEY-----`、`-----BEGIN RSA PRIVATE KEY-----`、`-----BEGIN EC PRIVATE KEY-----`或`-----BEGIN OPENSSH PRIVATE KEY-----`，或去除前导空白后以忽略大小写的`Bearer `、`Basic `开头时，把整个string替换为`[REDACTED]`；
- 允许`\n`、`\r`和`\t`，拒绝其他Unicode C0控制字符；
- 保证替换后仍满足OutputSchema；
- 使用语义截断，禁止按字节截断形成非法JSON；
- 规范化JSON后检查1 MiB上限；
- 不在错误摘要中回显原值；
- 不记录原始响应或完整Prompt。

无法安全结构化或脱敏时返回`ResultSanitizationFailed`；仅大小超限时返回`StepOutputTooLarge`。两者都不得把原始内容交给Task Runtime持久化。

Tool调用已经确定成功时，OutputSchema、安全处理或大小检查失败只改变Step结果，不得改写外部调用事实：返回`Failed`并携带`tool_result_update=COMPLETED`、`output=NULL`和对应Step错误。Tool调用确定失败携带`tool_result_update=FAILED`；写Tool结果未知携带`tool_result_update=UNKNOWN`。上述分支均不得遗留ToolExecution=RUNNING。

### 6.11 下一动作

Completed后：

- 存在sequence+1 Step时，Step Executor只返回目标`step_id`；Task Runtime结果事务锁定该Step后按第3.11节生成`EXECUTE_STEP`或`REQUEST_APPROVAL`并写入Checkpoint；
- 当前Step是Plan最后一个Step时，continuation=`FINALIZE_RUN`；
- 不从内存猜测Step，Task Runtime结果事务必须锁定Plan和下一Step重新校验；
- Step Executor不创建Checkpoint，也不把高风险Tool默认标记为`EXECUTE_STEP`；Task Runtime把生成后的continuation交给Checkpoint Manager。

### 6.12 Task Runtime结果协作

Task Runtime收到Outcome后：

- Completed：重新Guard并原子保存Step、适用的ToolExecution、Run Context和下一Checkpoint；
- WaitingApproval：确认这是已提交业务结果后立即返回Worker，不重复写状态；
- Failed：调用统一失败终态事务；写Tool Unknown同时更新ToolExecution；
- Stale：重新读取事实并丢弃结果；迟到Model结果最佳努力记录`LateModelResultIgnored`；
- 系统error：停止执行循环并交给Runtime Host。

结果事务失败时：

- Model/只读Tool确定结果且事务明确回滚、connection健康：按`RESULT_PERSISTENCE_FAILED`进入INTERRUPTED，允许人工Recover；
- 写Tool确定结果无法持久化：不重放，ToolExecution进入UNKNOWN并以`PersistenceAfterWriteFailed`终止；
- connection状态不确定：不补写，关闭Runtime，由下次StartupCleanup分类。

### 6.13 StartupCleanup在Tool边界前后的分类

下一Runtime Instance取得advisory lock后，StartupCleanup仅依据持久化事实分类，不读取Active Call Registry：

| 遗留现场 | 是否跨过外部副作用边界 | StartupCleanup收敛 | Recover |
|---|---:|---|---|
| Tool Step=Running、TaskExecution=RUNNING、当前版本不存在该动作的ToolExecution | 否 | TaskExecution→INTERRUPTED/WORKER_INTERRUPTED；Step、Task、Run保持Running；不创建Report | 允许 |
| 当前版本ToolExecution=RUNNING，且同一Execution Config的Tool Definition为只读 | 已进入可安全重做边界 | ToolExecution→FAILED/WORKER_INTERRUPTED；TaskExecution→INTERRUPTED；Step、Task、Run保持Running；不创建Report | 允许 |
| 当前版本ToolExecution=RUNNING，且同一Execution Config的Tool Definition为写入 | 是且结果未知 | ToolExecution→UNKNOWN、side_effect_unknown=true；Step、Run、TaskExecution、Task→FAILED/WRITE_TOOL_INTERRUPTED；创建或确认唯一Pending Report | 禁止 |
| 已完整提交WaitingApproval | 未在执行外部写调用 | 保持WaitingApproval，不按遗留RUNNING处理 | 不适用；等待审批命令 |
| 当前Recovery Start直接引用Approved Approval、Step=Running、尚无新版本ToolExecution | 否 | 保留直接Approval来源证据；TaskExecution→INTERRUPTED/WORKER_INTERRUPTED；Step、Task、Run保持Running；不创建Report | 允许 |

“不存在ToolExecution”必须同时检查task_id、step_id和当前execution_version。分类还必须读取当前版本最新Checkpoint、Step类型、同一Execution Config的静态Tool Definition及适用的不可变Approval；不得尝试从ToolExecution读取`read_only`。Checkpoint Manager只验证ToolExecution归属、status、error_code和side_effect_unknown等持久化后果。最大Checkpoint可安全归属但内容无效时按Task级`CheckpointInvalid`终止；对象归属本身不明或现场无法唯一分类时，StartupCleanup整体回滚并返回Runtime Fatal `PersistenceInvariantViolation`，Runtime不得启动。不得根据句柄不存在推断请求未发出。

## 7. 异常处理

### 7.1 StepError分类

| 作用域 | error_code | Outcome |
|---|---|---|
| 输入引用 | InputResolutionFailed | Failed |
| 模型输入 | ModelInputTooLarge | Failed |
| 模型调用 | ModelCallFailed | Failed |
| 模型输出 | ModelOutputInvalid | Failed |
| Tool输入 | ToolInputInvalid | Failed |
| Tool不存在 | ToolNotFound | Failed |
| Tool禁用 | ToolDisabled | Failed |
| Tool未授权 | ToolNotAuthorized | Failed |
| Tool开始时数据库UTC已到期 | TaskTimeout | Failed，Task Runtime使用Timeout终态事务 |
| 只读Tool超时 | ToolTimeout | Failed |
| 只读Tool连接失败 | ToolConnectionLost | Failed |
| Tool确定失败 | ToolCallFailed | Failed |
| 审批上下文变化 | ApprovalContextChanged | Failed |
| 当前Checkpoint无效 | CheckpointInvalid | Task Runtime在调用前发现时由Runtime终止；RequestApproval 仅在统一 Guard 通过、持锁复核确认可安全归属且共享 Policy 授权时由 Approval Manager 终止并返回 Terminalized |
| Tool成功但OutputSchema失败 | StepOutputInvalid | Failed，ToolExecution=COMPLETED |
| 结果安全处理 | ResultSanitizationFailed | Failed |
| Step结果过大 | StepOutputTooLarge | Failed |
| 写Tool结果未知 | WRITE_TOOL_INTERRUPTED或对应安全cause_code | Failed且side_effect_unknown=true |
| 已提交业务终态或所有权变化 | TASK_CANCELLED、TASK_TIMED_OUT、STALE_EXECUTION | Stale |
| Runtime停止 | RUNTIME_SHUTDOWN、LOCK_LOST | Stale |

除 `Terminalized` 已由 Approval Manager 提交终态外，Task Runtime使用上述error_code收敛当前Step、Run、TaskExecution和Task，并创建唯一Pending Report。错误摘要必须安全、有限且不包含原始外部响应。

Task Timeout 的字段语义固定为：

- Tool Framework返回`DeadlineExceeded(cause_code=TaskTimeout)`，Approval Manager返回`Conflict(cause_code=TaskTimeout)`；
- Step Executor只生成领域`Failed/TaskTimeout`，不得生成`error_code=TIMED_OUT`或`cause_code=TIMED_OUT`；
- Task Runtime提交终态时，Task、Run和活动Step使用`error_code=TaskTimeout`，TaskExecution使用`status=FAILED`、`termination_reason=TIMED_OUT`；
- `TASK_TIMED_OUT`仅是已提交超时终态后传播给进程内context的取消原因，不是持久化error_code/cause_code。

错误作用域固定如下：

| 事实 | 作用域 | 处理 |
|---|---|---|
| Kubernetes live resource与已批准冻结现场不同 | 当前Task | `ApprovalContextChanged`，不创建或确定失败ToolExecution |
| 当前版本最大Checkpoint缺失、不可解析、关联非法或Recovery来源证据不完整 | 当前Task | 调用前由 Task Runtime 发现时由 Runtime 终止；RequestApproval 仅在统一 Guard 通过、持锁复核确认可安全归属且共享 Policy 授权时由 Approval Manager 终止并返回 Terminalized；均不调用外部系统 |
| 已通过持久化校验后，调用DTO仍出现不可能的字段矛盾 | Runtime Fatal | `STEP_EXECUTOR_CONTRACT_BROKEN`，停止新Claim并关闭Runtime |
| 不可变静态配置投影自相矛盾 | Runtime Fatal | `RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT` |
| execution_version、worker_id、状态或deadline的合法竞争 | 当前调用Stale或TaskTimeout | 不归类为Runtime Fatal |

### 7.2 系统错误

以下错误不属于单Task失败：

- `STEP_EXECUTOR_CONTRACT_BROKEN`；
- `RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT`；
- `RUNTIME_INVALID_MODEL_CLIENT_REQUEST`；
- `PersistenceInvariantViolation`；
- 持锁connection失败或事务提交结果不确定。

Tool Framework的`RuntimeFatal`结果分支由Step Executor转换到自身独立system error通道；Tool Framework直接返回的非nil system error也沿同一路径传递。Task Runtime不得把它们转换为ModelCallFailed或ToolCallFailed；Worker停止新Claim，Runtime Host进入关闭流程。

### 7.3 Model Client错误映射

| Model Client类别 | StepError |
|---|---|
| Canceled且cause=TASK_CANCELLED/TASK_TIMED_OUT | Stale |
| Canceled且cause=ACTION_TIMEOUT | ModelCallFailed/MODEL_TIMEOUT |
| Canceled且cause=RUNTIME_SHUTDOWN/LOCK_LOST | Stale |
| Adapter本地timeout或Provider timeout | ModelCallFailed/MODEL_TIMEOUT |
| Authentication | ModelCallFailed/MODEL_AUTHENTICATION |
| RateLimited | ModelCallFailed/MODEL_RATE_LIMITED |
| Network | ModelCallFailed/MODEL_NETWORK |
| Provider | ModelCallFailed/MODEL_PROVIDER_ERROR |
| ResponseTooLarge | ModelCallFailed/MODEL_RESPONSE_TOO_LARGE |
| InvalidResponse | ModelOutputInvalid |
| ContractViolation | 系统error/RUNTIME_INVALID_MODEL_CLIENT_REQUEST |

业务cause优先于Adapter和Provider错误；禁止解析错误字符串分类。

### 7.4 Tool错误与副作用分类

| 场景 | ToolExecution | side_effect_unknown | StepError |
|---|---|---:|---|
| 开始事务Guard失败 | 不创建 | false | Stale |
| 开始事务确认Task已到期 | 不创建 | false | TaskTimeout |
| ACTION_TIMEOUT发生在ToolExecution创建前 | 不创建 | false | ToolTimeout |
| ACTION_TIMEOUT发生在只读Tool RUNNING后 | FAILED | false | ToolTimeout |
| ACTION_TIMEOUT发生在写Tool RUNNING后且尚未取得最终状态 | UNKNOWN | true | ToolTimeout |
| Patch明确2xx后ACTION_TIMEOUT或body处理失败 | COMPLETED，output=NULL | false | StepOutputInvalid或对应processing_error |
| Patch明确非2xx后错误body处理失败 | FAILED | false | 对应类型化Tool错误 |
| 只读Tool确定失败 | FAILED | false | ToolCallFailed |
| 只读Tooltimeout | FAILED | false | ToolTimeout |
| 只读Tool网络失败 | FAILED | false | ToolConnectionLost |
| 写Tool执行前复核变化 | 不创建 | false | ApprovalContextChanged |
| Patch原子resourceVersion test冲突 | FAILED | false | ApprovalContextChanged |
| Patch明确拒绝且确认未生效 | FAILED | false | ToolCallFailed |
| Patch确定成功 | COMPLETED | false | 无 |
| Patch发出后、最终状态取得前timeout、断连或结果不可确认 | UNKNOWN | true | WRITE_TOOL_INTERRUPTED或对应cause |
| Patch结果持久化失败 | UNKNOWN | true | PersistenceAfterWriteFailed |

Tool Framework Port分支到Step Executor作用域的映射固定为：

| Port分支 | Step Executor作用域 |
|---|---|
| `PreflightRejected` | 当前Step业务失败，不创建新的ToolExecution |
| `ToolBusinessFailed` | 当前Step业务失败；携带id时必须同时提交FAILED终态草案 |
| `CheckpointInvalid` | 当前Task失败，通过共享Task Lifecycle Policy收敛 |
| `RuntimeFatal` | system error，停止当前执行循环并交Runtime Host |
| 独立非nil `error` | system error，不猜测或降级为ToolCallFailed |

此映射与第3.9节共享结果表为同一契约；禁止由Fake、测试或具体Tool Runner覆盖。

### 7.5 重试规则

- Model Step不自动重试；
- Model输出非法不Repair；
- 只读Tool明确失败不自动重试；
- 写Tool任何失败或UNKNOWN不自动重试；
- resourceVersion冲突不刷新、不重新审批、不重试；
- Step Executor不重入当前Step；
- 只有Model/只读Tool安全中断后，由User Recover创建新execution_version才允许重新执行；
- 重复调用依赖持久化状态Guard返回Stale，不构成重试机制。

### 7.6 日志与审计

允许的最小结构化字段：

- task_id、run_id、step_id、execution_version；
- step_type、tool_name；
- result_kind、error_code；
- duration；
- tool_execution_id或approval_id；
- truncated、side_effect_unknown。

禁止记录：

- 完整Prompt、Task输入和Agent system prompt；
- 原始Model或Tool响应；
- 完整Tool input、Approval冻结参数或Kubernetes对象；
- Bearer Token、Model API Key、Kubernetes凭据；
- Provider原始错误和堆栈。

TaskLog不是恢复事实。TaskLog失败不得影响Step、ToolExecution或Checkpoint事务。

MVP事件的唯一Owner如下：

| 事件 | 唯一Owner | 记录时点 |
|---|---|---|
| `StepStarted` | Task Runtime | 动作开始事务提交后 |
| `StepCompleted`、`StepFailed` | Task Runtime | 结果或失败终态事务提交后 |
| `ToolRequested` | Tool Framework | ToolExecution=RUNNING事务提交后 |
| `ToolCompleted`、`ToolFailed`、`ToolResultUnknown` | Task Runtime | ToolExecution终态随结果/失败事务提交后 |
| `ApprovalRequested`、`ApprovalApproved`、`ApprovalRejected` | Approval Manager | 对应审批事务提交后 |
| `TaskTerminalized` | 实际终态事务Owner | 通常为Task Runtime；审批入口CheckpointInvalid由Approval Manager提交并记录 |
| `LateModelResultIgnored` | Task Runtime | 迟到结果Guard失败后 |

其他模块不得为同一事实重复发出同名TaskLog事件；Step Executor不拥有TaskLog事件。最佳努力TaskLog写入失败不重放领域动作，也不由另一个模块代写。

## 8. 并发与一致性

### 8.1 并发模型

- 单次ExecuteStep严格串行；
- 同一ExecuteStep只执行一个逻辑Model或Tool动作，不以底层Kubernetes API请求数定义动作次数；
- Model动作发出一个非流式模型请求；
- Get Deployment、Get Pod、Get Event和Get Container Log每个只读Tool动作最多发出一次Kubernetes API请求，不自动翻页；
- Approval准备最多发出一次Kubernetes GET；Approved Patch最多发出一次预检GET和一次带resourceVersion test的PATCH；
- 同一动作内禁止自动重试、分页循环、Patch后跟随核验或额外诊断请求；需要核验时必须由Plan中的独立Verification/read Tool Step执行；
- 不创建后台goroutine；
- context传播到Model Client、Tool Framework和Kubernetes Adapter；
- Step Executor不维护跨调用缓存或活动句柄；
- Active Call Registry由Task Runtime唯一拥有；
- MVP单Worker、单执行槽不替代数据库Guard。

### 8.2 事务边界

| 阶段 | 事务内 | 事务外 |
|---|---|---|
| Step动作开始 | Guard、Pending→Running | Active Call登记和输入纯计算 |
| Model调用 | 无 | Prompt构造、DeepSeek调用、解析、安全处理 |
| 只读Tool开始 | Guard、ToolExecution=RUNNING | Kubernetes调用、截断、脱敏 |
| Approval准备 | 无 | 输入解析、Deployment读取 |
| 进入Approval | Approval及全部等待状态、Checkpoint | 无外部调用 |
| 写Tool开始 | Guard、冻结输入、ToolExecution=RUNNING | 复核读取和Patch调用 |
| Step结果 | Step、ToolExecution、Run Context、Checkpoint | 原始结果处理 |
| 失败终态 | Step、Run、TaskExecution、Task、ToolExecution、Pending Report | 提交后取消句柄和最佳努力日志 |

所有事务使用持锁connection、READ COMMITTED、显式锁和预期状态条件；禁止嵌套事务。

### 8.3 Version Guard

动作开始、Tool开始和结果事务至少匹配：

- task_id、run_id、step_id；
- Task.current_execution_version；
- TaskExecution.execution_version和RUNNING；
- TaskExecution.worker_id；
- Run.current_step_id和Running；
- Step预期状态；
- 适用时ToolExecution预期RUNNING；
- Runtime仍持有写连接。

已批准写Tool还必须匹配当前Checkpoint直接引用的Approval ID。当前Checkpoint和新ToolExecution必须匹配当前execution_version；Approval版本按同版本或第4.4节Recovery跨版本规则校验，不得笼统要求Approval.execution_version等于当前版本。

### 8.4 主要竞态

| 竞态 | 结果 |
|---|---|
| Step动作开始 vs Cancel/Timeout | 由事务提交顺序和Guard决定；终止先提交时不调用外部系统 |
| Approval准备读取 vs Cancel/Timeout | Approval事务Guard失败，丢弃读取结果 |
| Approval等待事务 vs Cancel/Timeout | 持锁写通道按提交顺序决定唯一状态 |
| Approve vs旧Worker | WaitingApproval已清worker_id；旧Worker不得继续 |
| 新Claim vs approved执行 | 只有成功领取同版本Execution的新Worker可以调用 |
| Patch复核 vs资源变化 | 同一Patch请求中的resourceVersion test原子拒绝 |
| ToolExecution开始 vs Cancel/Timeout | 终止先提交则不创建；写Tool开始先提交则终止写UNKNOWN |
| 确定Tool结果 vs Cancel/Timeout | 结果先提交保留确定事实；终止先提交拒绝迟到结果 |
| Model结果 vs Recover新版本 | 旧execution_version结果Stale |
| Runtime失锁 vs外部结果 | 旧进程无写通道，不得提交 |
| 写Tool成功 vs结果事务失败 | 不重放，转UNKNOWN或由StartupCleanup分类 |

### 8.5 幂等边界

- Step Executor没有command_id，不是User命令入口；
- 同一Step不创建内部幂等Receipt；
- Approval命令幂等由Approval Manager的Command Receipt提供；
- ToolExecution每次实际调用按execution_version创建；
- 恢复的只读Tool创建新的ToolExecution；
- Approval后继续沿用同一execution_version，但只执行同一不可变Approved Approval；
- Approval后发生安全Recover时，新execution_version继续引用原不可变Approved Approval，不复制Approval；
- 写Tool不承诺exactly-once，不通过本地请求ID自动重放。

### 8.6 恢复一致性

- Step业务结构不随execution_version复制；
- Model/只读Tool恢复继续同一Running Step；
- 当前版本Recovery Start Checkpoint必须自包含；
- Executor只消费当前版本Checkpoint直接事实，不跨版本遍历；
- 已完成Step只作为紧邻引用来源，不重新执行；
- Approval后Recover的新版本起点直接携带已验证Approved Approval引用、source_execution_version、source_checkpoint_id和冻结参数；
- Recover事务负责验证完整历史来源并把验证结果物化到新起点；Worker和Step Executor只校验当前起点及其直接Approval引用；
- 当前版本尚无ToolExecution的Approved写Tool恢复现场仍处于副作用边界之前，可再次人工Recover；
- UNKNOWN写Tool禁止Recover。

### 8.7 数据安全一致性

- 原始外部响应只在当前调用内存存在；
- Tool Framework先截断和脱敏，Step Executor再执行输出投影和最终防线；
- 只有safe_output可进入Step、ToolExecution、Checkpoint或后续Model；
- Checkpoint只引用已持久化结果，不复制原始响应；
- Provider request ID只允许进程内安全日志关联；
- 失锁后旧进程不得写TaskLog或任何领域结果。

### 8.8 配置一致性

- Step Executor不计算或比较execution_config_hash；
- Task Runtime在Claim前已经完成TaskExecution、适用Checkpoint和当前`ExecutionConfigV1`的三方hash门禁；
- 当前调用中的Agent、Model、JSON、安全、Step和Tool投影必须来自计算该hash的同一不可变`ExecutionConfigV1`实例；
- ToolCall的AgentAuthorization和allowed_tools必须来自同一实例；
- Step Executor依赖的唯一字段范围是`agent`、`model`、`json`、`safety`、`step_executor`和适用的`tool_framework`投影；
- GenerationParams、Step输入/引用协议、action mode、OutputSchema、JSON与结果限制逐字段引用共享契约第5节和第7.4～7.5节，不在本模块追加hash字段；
- Step Executor不得在调用中重新加载另一份Agent或Tool配置；
- 凭证、API endpoint、DNS、TLS和日志级别等排除项以`ExecutionConfigV1`唯一规则为准；
- 运行期快照不一致是系统错误，不允许使用“当前新配置”继续旧TaskExecution。

## 9. 测试场景

### 9.1 Input Resolver单元测试

| ID | 场景 | 预期 |
|---|---|---|
| SE-IR-001 | sequence=2引用前一步直接字段 | 成功深拷贝实际值 |
| SE-IR-002 | sequence=1含引用 | InputResolutionFailed |
| SE-IR-003 | previous_step不是sequence-1 | InputResolutionFailed |
| SE-IR-004 | previous_step不是Completed | InputResolutionFailed |
| SE-IR-005 | output_schema无字段 | InputResolutionFailed |
| SE-IR-006 | 实际output无字段或为null | InputResolutionFailed |
| SE-IR-007 | 实际类型与来源声明不符 | InputResolutionFailed |
| SE-IR-008 | 来源类型与目标位置不兼容 | InputResolutionFailed |
| SE-IR-009 | integer赋给number | 允许 |
| SE-IR-010 | `step.output.a.b`、数组下标或模板 | InputResolutionFailed |
| SE-IR-011 | 普通文本中部包含`step.output.a` | 按字面量处理 |
| SE-IR-012 | 以保留前缀开头但非法 | InputResolutionFailed |
| SE-IR-013 | 替换后ModelCall/Analysis/Verification契约非法 | InputResolutionFailed |
| SE-IR-014 | 替换后Tool required或Schema非法 | InputResolutionFailed |
| SE-IR-015 | 深度或字段数超限 | InputResolutionFailed |
| SE-IR-016 | 请求resolved_references相对共享提取结果遗漏、额外、重复或顺序不同 | STEP_EXECUTOR_CONTRACT_BROKEN，不调用外部系统 |
| SE-IR-017 | target_path含非法segment或超过16层 | STEP_EXECUTOR_CONTRACT_BROKEN及稳定引用cause |
| SE-IR-017A | 单Step含257条引用 | STEP_EXECUTOR_CONTRACT_BROKEN/REFERENCE_COUNT_LIMIT_EXCEEDED，不调用外部系统 |
| SE-IR-018 | 与Task Runtime、Checkpoint Manager对同一Step.input运行共享提取器 | 三方得到逐字段相同的CanonicalResolvedReferences |
| SE-IR-019 | GENERATE_PLAN或FINALIZE_RUN被传入ExecuteStep | STEP_EXECUTOR_CONTRACT_BROKEN；NO_STEP_INPUT不属于Step Executor |

### 9.2 Model Step测试

- 三类Step使用固定输入模板和同一Model Client；
- Planner INITIAL/REPAIR与三类Model Step共享同一强类型GenerationParams；
- 缺省值规范化、字段范围、未知字段拒绝和固定序列化顺序由Task Runtime的`ExecutionConfigV1`构造器执行；
- GenerationParams任一语义字段变化时仅Task Runtime重算execution_config_hash，Step Executor只验证投影仍来自同一实例；
- 共享ExecutionConfigV1 fixture的hash固定为`27f26b3d37da75facddaca5b9cbc23de91093977d75746fb5c53a0d92d257f43`，Step Executor取得的model/json/safety/step_executor投影逐字段一致；
- Adapter不能映射参数时启动失败，运行期投影矛盾为Runtime Fatal；
- 只注入ResolvedStepInput，不注入历史、TaskLog或Memory；
- model=`deepseek-chat`、stream=false、response_format=json_object；
- Prompt恰为256 KiB通过，超过时不调用模型；
- 单次timeout为min(60s，上层剩余时间)；
- Provider、认证、限流、网络和timeout不重试；
- ACTION_TIMEOUT收敛为ModelCallFailed；
- TASK_CANCELLED/TASK_TIMED_OUT优先返回Stale；
- Runtime Shutdown/Lock Lost不提交结果；
- 合法JSON object严格匹配OutputSchema；
- 缺字段、额外字段、null、重复key、类型错误和超过1 MiB均失败；
- Model输出非法不执行Repair。

### 9.3 只读Tool测试

- Tool存在、enabled、allowed、Low和read_only才自动执行；
- allowed_tools仅从请求AgentAuthorization读取，执行期不得重新加载Agent配置；
- Tool input替换后执行完整Schema校验；
- 未允许cluster或namespace被拒绝；
- 每个只读Tool动作最多发出一次Kubernetes API请求且不自动翻页；
- ToolExecution=RUNNING先于Kubernetes调用提交；
- 开始Guard失败不创建ToolExecution、不调用Kubernetes；
- Tool开始事务确认数据库deadline已到时不创建ToolExecution，并由Task Runtime按TaskTimeout收敛；
- Tool Framework的DeadlineExceeded与Approval Manager的超时Conflict均携带cause_code=TaskTimeout，Step Executor映射为Failed/TaskTimeout；
- Step Executor不会产生error_code=TIMED_OUT或cause_code=TIMED_OUT；TaskExecution.termination_reason=TIMED_OUT只由Task Runtime超时事务写入；
- ACTION_TIMEOUT在只读Tool边界后写FAILED/ToolTimeout，在写Tool边界后写UNKNOWN/ToolTimeout；
- 结果超过1 MiB执行语义截断并记录元数据；
- Container Log默认200行、超过1000行拒绝；
- timeout和网络失败写FAILED，不产生UNKNOWN；
- Tool确定成功但OutputSchema、安全处理或大小检查失败时，ToolExecution收敛为COMPLETED、output=NULL，Step按对应错误失败；
- Tool确定失败收敛为FAILED，任一已创建ToolExecution的Failed Outcome均携带终态更新；
- Worker中断后由StartupCleanup标记WORKER_INTERRUPTED并允许人工Recover；
- 恢复创建不同execution_version的新ToolExecution。

### 9.4 Approval与Patch测试

- Plan首个Step为High/write时，初始Checkpoint的next_action是REQUEST_APPROVAL；
- 已完成Step的后继为High/write时，结果事务生成REQUEST_APPROVAL而不是EXECUTE_STEP；
- Approval路径严格执行Pending→Running→WaitingApproval，输入解析失败从Running收敛为Failed；
- Cancel/Timeout与动作开始、Approval等待事务按提交顺序竞争，不允许跳过Running或留下半等待现场；
- 参数非法不读取Kubernetes、不创建Approval或ToolExecution；
- REQUEST_APPROVAL读取并冻结旧值、新值和resourceVersion；
- WaitingApproval事务原子包含Approval、Step、Task、Run、TaskExecution、Checkpoint和清队列；
- Approval Manager返回非超时`Conflict`时Executor映射为Stale且不补写状态；
- Step Executor 只按 Approval 模块的 `RequestApprovalCommand` 调用，不存在位置参数入口或本地 FrozenToolRequest 字段列表；
- `Pending`与`Existing`均映射WaitingApproval；`Existing`不触发第二次等待事务；
- RequestApproval 的统一 Guard 全部通过、Checkpoint 可安全归属但无效且 Task Lifecycle Policy 允许时，Approval Manager 原子终止 Task 并创建 Report，Executor 返回 Terminalized 且不返回 Failed；
- 坏 Checkpoint 与旧 execution_version、错误 worker、状态竞争、deadline 到期或错误 FrozenToolRequest 组合时，Approval Manager 返回对应 `Conflict` 或 `RuntimeFatal`，Executor 不得返回 Terminalized；
- Approval Manager返回的超时`Conflict`必须携带cause_code=TaskTimeout，Executor不得将其改写为TIMED_OUT；
- RequestApproval发现Checkpoint对象归属不明时返回`RuntimeFatal(PersistenceInvariantViolation)`；
- Approval Manager返回`ApprovalRequestResult.CheckpointInvalid`时Executor返回Terminalized，Task Runtime不重复失败或创建Report；
- Approve后只使用冻结input，不重新解析Step.input；
- 同版本Approved Approval由`APPROVED_CONTINUATION` Evidence直接引用，source两字段为空；
- Recover后的当前Recovery Start Checkpoint可直接引用旧版本Approved Approval，不复制Approval、不遍历历史Checkpoint；
- 跨版本Evidence缺少source_execution_version、source_checkpoint_id或不能证明直接Approval引用时按CheckpointInvalid终止；
- ApprovedAction与Evidence的approval_id、frozen_input_hash、execution_config_hash或版本模式矛盾时返回STEP_EXECUTOR_CONTRACT_BROKEN；
- ApprovedAction不含Checkpoint/source字段，Evidence不含完整冻结动作；Fake记录值也必须保持该边界；
- 同版本执行在进程重启后仍从数据库 Approval、TaskExecution、Checkpoint 验证三个 hash，不依赖 FrozenToolRequest；
- Approval hash 与其所属版本 TaskExecution 不一致时传播 PersistenceInvariantViolation；Approval/TaskExecution 一致但 Checkpoint hash 不一致时传播 CheckpointInvalid，均不调用 Tool Framework；
- 执行前字段或resourceVersion变化时不创建ToolExecution；
- 复核后资源变化由同一JSON Patch的resourceVersion test拒绝；
- Patch请求只含resourceVersion test和允许的replicas/image操作；
- resourceVersion冲突确定FAILED/ApprovalContextChanged且不重试；
- 最终状态取得前timeout、断连和响应未知进入UNKNOWN、side_effect_unknown=true；
- 写Tool context在RUNNING边界后、最终状态取得前取消返回`SideEffectUnknown`；最终状态已取得时保持`InvocationCompleted`或`ToolBusinessFailed`；
- Patch明确2xx后的body读取、解析、脱敏或大小失败在正常结果事务中保持ToolExecution=COMPLETED、output=NULL；
- Patch明确非2xx且错误body不可解析时保持`ToolBusinessFailed`；
- Reject不调用Step Executor写路径、不创建ToolExecution；
- Patch后Verification只确认目标字段，不表述rollout健康；
- Approved Patch单次逻辑动作最多一次预检GET和一次PATCH，不进行自动核验或重试。

### 9.5 Runtime编排与事务测试

- Worker不直接调用Step Executor；
- Task Runtime动作开始事务先于外部调用；
- Active Call Registry预登记、激活和调用前context检查顺序正确；
- Completed结果事务原子保存Step、ToolExecution、Run Context和Checkpoint；
- 首Step和后继Step使用同一个next_action生成规则，Worker、Executor和Checkpoint Manager均不重新推导；
- WaitingApproval返回后Task Runtime不重复写状态；
- Terminalized返回后Task Runtime只确认已提交终态并结束循环，不重复执行失败事务；
- Failed结果原子终止并创建唯一Pending Report；
- Stale不推进下一Step；
- 旧execution_version或worker_id结果被拒绝；
- Cancel/Timeout和Tool结果按提交顺序竞争；
- Model/只读结果持久化明确回滚时进入INTERRUPTED；
- 写Tool结果持久化失败进入UNKNOWN且不重放；
- 持锁connection错误返回系统error并停止Runtime；
- Kubernetes live resource变化只产生ApprovalContextChanged；
- 当前最大Checkpoint无效按CheckpointInvalid终止当前Task，不进入Step Executor；
- 当前最大Checkpoint缺失固定为CheckpointInvalid/CHECKPOINT_NOT_FOUND，不进入Step Executor且不改判DataInconsistent；
- Step Executor仅消费并复核当前Checkpoint canonical resolved_references，不构造下一Checkpoint绑定；
- 已校验DTO矛盾或静态配置快照矛盾返回Runtime Fatal；
- 合法Guard竞争返回Stale，不升级Runtime Fatal；
- Tool起始事务AlreadyStarted返回PersistenceInvariantViolation，不调用Kubernetes、不覆盖已有ToolExecution；
- 任一数据库事务内不调用DeepSeek或Kubernetes；
- StartupCleanup遇到Running Tool Step且无当前版本ToolExecution时收敛为INTERRUPTED并允许Recover；
- StartupCleanup分别覆盖ToolExecution=RUNNING且Registry定义为只读、ToolExecution=RUNNING且Registry定义为写入、完整WaitingApproval和Approved Recovery Start但无新ToolExecution；
- StartupCleanup的只读/写入分类来自同一静态Tool Definition；ToolExecution DTO、Repository投影和Checkpoint ValidationFacts均不存在read_only；
- StartupCleanup分类不依赖Active Call Registry，模糊现场不得猜测副作用边界。

### 9.6 安全与日志测试

- Model和Tool原始响应不进入数据库、Checkpoint、TaskLog或Report；
- OutputSchema之外字段不进入Step.output；
- 敏感键和常见凭据被替换；
- 脱敏失败不返回原始值；
- Tool截断保持合法JSON；
- safe output超过1 MiB时失败；
- safe_summary不超过512 UTF-8字节，结构化日志string字段不超过256字节，截断保留合法UTF-8且不回显原始值；
- 日志不含Prompt、完整Tool input、Approval参数、原始响应或凭据；
- TaskLog失败不改变领域事务；
- 每个TaskLog事件只有第7.6节指定Owner，其他模块不重复发出。

### 9.7 Tool Framework Port单元与契约测试

- 公开接口和Fake只包含三个类型化入口；编译期检查不存在`ValidateCapability`、`ToolCapabilityRequest`或`ToolFrameworkError`跨包引用；
- `InvokeReadTool`只接收一个完整`ReadToolRequest`，Fake记录的scope、authorization、tool_name、resolved_input、tool_definition和deadline_at逐字段相等；
- `PrepareWriteApproval`只接收一个完整`PrepareWriteApprovalRequest`，成功固定返回`ApprovalPrepared{FrozenToolRequest}`，不存在直接返回FrozenToolRequest的接口或Fake捷径；
- `InvokeApprovedWrite`只接收一个完整`ApprovedWriteRequest`，其中ApprovedAction、ApprovedCheckpointEvidence和StaticToolDefinition均未被Step Executor重新计算；
- 三个入口记录的 `scope.execution_config_hash` 与入站 StepExecutionRequest 完全相等，覆盖正常只读、审批准备和已批准写 Tool；
- 入站 Scope hash 为空或格式非法时返回 `STEP_EXECUTOR_CONTRACT_BROKEN`，Fake/真实 Tool Framework 均未被调用；
- 出站 DTO 的 Scope hash 被构造器、Adapter 或测试故障注入改写时，Step Executor 在调用前拒绝并返回 `STEP_EXECUTOR_CONTRACT_BROKEN`；
- `ApprovedWriteRequest`中的Action、Evidence和scope execution_config_hash逐字段相等，Action/Evidence的approval_id和frozen_input_hash相等；Recover路径覆盖直接source版本；
- Fake深拷贝Action与Evidence，不允许把Checkpoint/source字段写入Action或把完整冻结输入写入Evidence；
- 三个方法均拒绝散列参数、`map[string]any`请求和本地重复DTO；
- 对每个方法穷尽测试其允许的`ToolFrameworkResult`分支及第3.9节的StepOutcome/error映射；
- `PreflightRejected`不创建ToolExecution并映射业务Failed；
- `ToolBusinessFailed`有、无ToolExecution两种边界均正确生成终态草案；
- `CheckpointInvalid`映射Task级失败，不升级为Runtime关闭；
- 三个Tool入口的`DeadlineExceeded`和Approval入口的超时`Conflict`均只接受`cause_code=TaskTimeout`并映射`Failed/TaskTimeout`；出现`TIMED_OUT` error/cause视为契约破坏；
- `RuntimeFatal`和非nil system error均进入Step Executor system error通道，不转换为ToolCallFailed；
- 结果与error同时存在、同时为空或方法返回不允许分支时，固定转换为`STEP_EXECUTOR_CONTRACT_BROKEN`；
- `SideEffectUnknown`只允许Approved写入口，携带UNKNOWN终态更新和`side_effect_unknown=true`；
- Fake按FIFO返回、深拷贝请求、观察context取消且不产生真实外部副作用；
- Port契约测试对Step Executor Fake与Tool Framework真实实现运行同一组请求/结果兼容性用例，确保两端共享同一类型定义。

### 9.8 Approval Request Port 契约测试

- 编译期只存在 Approval 模块定义的 `RequestApproval(ctx, RequestApprovalCommand) (ApprovalRequestResult, error)`；Step Executor 不声明位置参数重载、本地 Command 或本地 Result；
- Command 的 Scope、FrozenRequest、StepID、ExecutionConfigHash、ApprovalContext 与入站 StepExecutionRequest 和 Tool Framework 结果逐字段一致；
- 显式 ExecutionConfigHash、Scope hash、FrozenRequest hash 任一为空或不相等时，不调用 Fake/真实 Approval Manager，并返回 `STEP_EXECUTOR_CONTRACT_BROKEN`；
- `ApprovalContext` 固定为 `REQUEST_APPROVAL + 同一tool_name + High + write`，不存在调用方任意 Map；
- `Pending`和`Existing`均映射为WaitingApproval，且Step Executor不补写Approval、Checkpoint或四对象状态；
- `Conflict(cause_code=TaskTimeout)`映射为Failed/TaskTimeout，其他Conflict映射为Stale；
- `CheckpointInvalid`映射为Terminalized，Task Runtime不重复提交终态或Pending Report；
- `RuntimeFatal`和非nil system error均进入system error通道；
- 未知分支、结果与error同时存在或同时为空固定转换为`STEP_EXECUTOR_CONTRACT_BROKEN`；
- `FakeApprovalRequestPort`按FIFO返回、深拷贝完整Command、记录context取消状态，不展开FrozenToolRequest或执行真实事务；
- Step Executor Fake 与 Approval Manager 实现运行同一契约套件，覆盖五个结果分支和独立error通道。

### 9.9 验收标准映射

Timeout跨模块验收必须同时断言：Tool边界返回`DeadlineExceeded(cause_code=TaskTimeout)`，Approval边界返回`Conflict(cause_code=TaskTimeout)`；Step Executor输出`Failed/TaskTimeout`；Task Runtime提交后Task/Run/活动Step为`error_code=TaskTimeout`且TaskExecution为`termination_reason=TIMED_OUT`；所有领域error_code/cause_code均不出现`TIMED_OUT`。

| 需求验收 | 本设计覆盖 |
|---|---|
| AC-PLAN-05～07 | 紧邻引用运行期存在性、类型、表达式拒绝 |
| AC-TOOL-01～05 | 只读Tool路由、权限、结果和无重试 |
| AC-TOOL-06～09 | ToolExecution边界、截断脱敏、UNKNOWN |
| AC-TOOL-10～11 | replicas范围和Registry白名单 |
| AC-APP-01～04 | Approval冻结、等待事务、Approve继续和Reject不执行 |
| AC-APP-06～09 | Patch范围、resourceVersion和终态Approval |
| AC-APP-11～12 | Patch后Verification和同版本继续 |
| AC-CP-01～05 | Step结果Checkpoint、审批边界和恢复 |
| AC-CP-07、10～11 | Model/只读恢复、结果事务和写结果失败 |
| AC-TASK-05～08、12、14～15 | 取消、超时、Tool边界、版本Guard和短事务 |
| AC-LOG-01～04 | 安全结果、最小日志和迟到结果 |
| AC-E2E-01～08 | 只读诊断、审批Patch、验证、恢复和UNKNOWN |

## 10. 待确认问题

无。GenerationParams使用共享Model Client强类型；全部执行语义的唯一规范化结构和hash规则冻结在《跨模块共享领域契约》第5节，Task Runtime 是唯一计算 Owner，本模块不维护局部hash契约。

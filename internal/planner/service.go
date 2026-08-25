package planner

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const (
	plannerModelName          = "deepseek-chat"
	plannerResponseFormat     = "json_object"
	plannerModelCallTimeout   = 60 * time.Second
	repairMinModelBudget      = 15 * time.Second
	plannerLocalSafetyMargin  = 2 * time.Second
	maxTaskInputBytes         = 16 * 1024
	maxAgentSystemPromptBytes = 32 * 1024
	maxPlannerTools           = 32
)

// PlannerPort 是 Planner 模块拥有的入站应用契约。
type PlannerPort interface {
	GeneratePlan(context.Context, PlannerRequest) (ValidatedPlanDraft, error)
}

// PlannerRequest 是 Task Runtime 传给 Planner 的不可变调用事实。
// 它不包含 Repository、事务、持久化 Entity 或 Provider SDK 类型。
type PlannerRequest struct {
	TaskID              contracts.TaskID
	RunID               contracts.RunID
	ExecutionVersion    contracts.ExecutionVersion
	TaskInput           string
	AgentID             contracts.AgentID
	AgentSystemPrompt   string
	AllowedTools        []string
	MaxSteps            uint32
	ModelName           string
	GenerationParams    contracts.GenerationParams
	ExecutionConfigHash contracts.ExecutionConfigHash
	ToolCatalogSelector contracts.PlanningToolCatalogSelector
}

// ValidatedPlanDraft 是尚未持久化、但已经通过完整 Planner 校验的结果。
type ValidatedPlanDraft struct {
	TaskID           contracts.TaskID
	RunID            contracts.RunID
	ExecutionVersion contracts.ExecutionVersion
	Goal             string
	Steps            []StepDraft
}

// PlannerErrorKind 是 GeneratePlan 的封闭失败类别。
type PlannerErrorKind string

const (
	PlannerErrorRuntimeFatal         PlannerErrorKind = "RuntimeFatalError"
	PlannerErrorCanceled             PlannerErrorKind = "PlannerCanceled"
	PlannerErrorPlanGenerationFailed PlannerErrorKind = "PlanGenerationFailed"
	PlannerErrorPlanValidationFailed PlannerErrorKind = "PlanValidationFailed"
)

// Valid 报告 PlannerErrorKind 是否属于冻结结果集合。
func (k PlannerErrorKind) Valid() bool {
	switch k {
	case PlannerErrorRuntimeFatal, PlannerErrorCanceled,
		PlannerErrorPlanGenerationFailed, PlannerErrorPlanValidationFailed:
		return true
	default:
		return false
	}
}

// PlannerPhase 标识失败发生于首次调用或唯一修复调用。
type PlannerPhase string

const (
	PlannerPhaseInitial PlannerPhase = "INITIAL"
	PlannerPhaseRepair  PlannerPhase = "REPAIR"
)

// Valid 报告 PlannerPhase 是否属于冻结调用阶段。
func (p PlannerPhase) Valid() bool {
	return p == PlannerPhaseInitial || p == PlannerPhaseRepair
}

// PlannerCauseCode 是 Planner 对外公开的稳定、安全原因码。
type PlannerCauseCode string

const (
	PlannerCauseRuntimeInvalidRequest                 PlannerCauseCode = "RUNTIME_INVALID_PLANNER_REQUEST"
	PlannerCauseRuntimeInvalidModelClientRequest      PlannerCauseCode = "RUNTIME_INVALID_MODEL_CLIENT_REQUEST"
	PlannerCauseRuntimeStaticToolSnapshotInconsistent PlannerCauseCode = "RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT"
	PlannerCauseRuntimeContractBroken                 PlannerCauseCode = "RUNTIME_PLANNER_CONTRACT_BROKEN"
	PlannerCauseRuntimePromptInvariantBroken          PlannerCauseCode = "RUNTIME_PROMPT_INVARIANT_BROKEN"
	PlannerCauseTaskCancelled                         PlannerCauseCode = "TASK_CANCELLED"
	PlannerCauseTaskTimedOut                          PlannerCauseCode = "TASK_TIMED_OUT"
	PlannerCauseActionTimeout                         PlannerCauseCode = "ACTION_TIMEOUT"
	PlannerCauseRuntimeShutdown                       PlannerCauseCode = "RUNTIME_SHUTDOWN"
	PlannerCauseLockLost                              PlannerCauseCode = "LOCK_LOST"
	PlannerCauseTaskInputTooLarge                     PlannerCauseCode = "TASK_INPUT_TOO_LARGE"
	PlannerCauseModelCallTimeout                      PlannerCauseCode = "MODEL_CALL_TIMEOUT"
	PlannerCauseModelProviderTimeout                  PlannerCauseCode = "MODEL_PROVIDER_TIMEOUT"
	PlannerCauseModelAuthentication                   PlannerCauseCode = "MODEL_AUTHENTICATION"
	PlannerCauseModelNetwork                          PlannerCauseCode = "MODEL_NETWORK"
	PlannerCauseModelRateLimited                      PlannerCauseCode = "MODEL_RATE_LIMITED"
	PlannerCauseModelProviderError                    PlannerCauseCode = "MODEL_PROVIDER_ERROR"
	PlannerCauseModelResponseTooLarge                 PlannerCauseCode = "MODEL_RESPONSE_TOO_LARGE"
	PlannerCauseRepairExhausted                       PlannerCauseCode = "REPAIR_EXHAUSTED"
	PlannerCauseRepairBudgetInsufficient              PlannerCauseCode = "REPAIR_BUDGET_INSUFFICIENT"
	PlannerCauseRepairPromptTooLarge                  PlannerCauseCode = "REPAIR_PROMPT_TOO_LARGE"
)

// Valid 报告 PlannerCauseCode 是否属于冻结安全原因集合。
func (c PlannerCauseCode) Valid() bool {
	switch c {
	case PlannerCauseRuntimeInvalidRequest, PlannerCauseRuntimeInvalidModelClientRequest,
		PlannerCauseRuntimeStaticToolSnapshotInconsistent, PlannerCauseRuntimeContractBroken,
		PlannerCauseRuntimePromptInvariantBroken, PlannerCauseTaskCancelled,
		PlannerCauseTaskTimedOut, PlannerCauseActionTimeout, PlannerCauseRuntimeShutdown,
		PlannerCauseLockLost, PlannerCauseTaskInputTooLarge, PlannerCauseModelCallTimeout,
		PlannerCauseModelProviderTimeout, PlannerCauseModelAuthentication,
		PlannerCauseModelNetwork, PlannerCauseModelRateLimited, PlannerCauseModelProviderError,
		PlannerCauseModelResponseTooLarge, PlannerCauseRepairExhausted,
		PlannerCauseRepairBudgetInsufficient, PlannerCauseRepairPromptTooLarge:
		return true
	default:
		return false
	}
}

// CancellationCause 是调用方可注入 context.WithCancelCause 的冻结 Planner 取消原因。
type CancellationCause PlannerCauseCode

func (c CancellationCause) Error() string { return string(c) }

// PlannerCancellationCause 使取消分类不依赖错误文本。
func (c CancellationCause) PlannerCancellationCause() PlannerCauseCode {
	return PlannerCauseCode(c)
}

// Valid 报告 CancellationCause 是否是 Task Runtime 可注入的冻结业务取消原因。
func (c CancellationCause) Valid() bool {
	switch PlannerCauseCode(c) {
	case PlannerCauseTaskCancelled, PlannerCauseTaskTimedOut, PlannerCauseActionTimeout,
		PlannerCauseRuntimeShutdown, PlannerCauseLockLost:
		return true
	default:
		return false
	}
}

// PlannerError 不携带 Task 输入、Prompt、模型响应或 Provider 原始错误。
type PlannerError struct {
	Kind        PlannerErrorKind
	CauseCode   PlannerCauseCode
	Phase       PlannerPhase
	Issues      []RepairIssue
	CatalogKind *contracts.PlanningToolCatalogErrorKind
	cause       error
}

func (e *PlannerError) Error() string {
	if e == nil {
		return "planner error"
	}
	return "planner error: " + string(e.Kind) + "/" + string(e.CauseCode) + "/" + string(e.Phase)
}

// Unwrap 仅保留标准 Context 取消语义，不暴露 Provider 或其他底层错误。
func (e *PlannerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Service 编排一次无数据库副作用的 Plan 生成。
type Service struct {
	model            contracts.ModelClient
	catalog          CatalogConsumer
	parser           Parser
	verify           Validator
	prompt           PromptBuilder
	modelCallTimeout time.Duration
}

var _ PlannerPort = (*Service)(nil)

// NewService 创建 Planner Application Service。
func NewService(model contracts.ModelClient, catalog contracts.PlanningToolCatalogPort) *Service {
	return &Service{
		model: model, catalog: NewCatalogConsumer(catalog), parser: NewParser(),
		verify: NewValidator(), prompt: NewPromptBuilder(), modelCallTimeout: plannerModelCallTimeout,
	}
}

// GeneratePlan 串行执行 INITIAL 和至多一次 REPAIR，并只返回完整成功结果或 PlannerError。
func (s *Service) GeneratePlan(ctx context.Context, request PlannerRequest) (ValidatedPlanDraft, error) {
	if err := validatePlannerRequest(ctx, request); err != nil {
		return ValidatedPlanDraft{}, err
	}
	if s == nil || s.model == nil || s.catalog.port == nil {
		return ValidatedPlanDraft{}, plannerError(
			PlannerErrorRuntimeFatal, PlannerCauseRuntimeContractBroken, PlannerPhaseInitial, nil,
		)
	}
	if canceled := plannerCancellation(ctx, PlannerPhaseInitial); canceled != nil {
		return ValidatedPlanDraft{}, canceled
	}

	snapshot, err := s.catalog.Load(ctx, request.ToolCatalogSelector)
	if err != nil {
		if canceled := plannerCancellation(ctx, PlannerPhaseInitial); canceled != nil {
			return ValidatedPlanDraft{}, canceled
		}
		return ValidatedPlanDraft{}, mapCatalogError(err)
	}
	initial := InitialPromptRequest{
		AgentSystemPrompt: request.AgentSystemPrompt,
		TaskInput:         request.TaskInput,
		ToolSnapshot:      snapshot,
		MaxSteps:          request.MaxSteps,
	}
	messages, err := s.prompt.BuildInitial(initial)
	if err != nil {
		return ValidatedPlanDraft{}, plannerError(
			PlannerErrorRuntimeFatal, PlannerCauseRuntimePromptInvariantBroken, PlannerPhaseInitial, nil,
		)
	}

	response, modelErr := s.generate(ctx, request, messages, PlannerPhaseInitial, s.callTimeout())
	if modelErr != nil {
		if isRepairableModelError(modelErr) {
			return s.repair(ctx, request, snapshot, initial, nil, []RepairIssue{{
				Code: string(ParseIssueInvalidJSON), Path: "$", Summary: string(ParseIssueInvalidJSON),
			}})
		}
		return ValidatedPlanDraft{}, modelErr
	}
	draft, issues, parsed := s.validateCandidate(response.AssistantContent, request, snapshot)
	if len(issues) == 0 {
		if canceled := plannerCancellation(ctx, PlannerPhaseInitial); canceled != nil {
			return ValidatedPlanDraft{}, canceled
		}
		return validatedResult(request, draft), nil
	}
	var candidate *PlanDraft
	if parsed {
		candidate = &draft
	}
	return s.repair(ctx, request, snapshot, initial, candidate, issues)
}

func validatePlannerRequest(ctx context.Context, request PlannerRequest) error {
	if ctx == nil || request.TaskID == "" || request.RunID == "" || !request.ExecutionVersion.Valid() ||
		request.AgentID == "" || !utf8.ValidString(string(request.AgentID)) ||
		strings.TrimSpace(request.TaskInput) == "" || !utf8.ValidString(request.TaskInput) ||
		strings.TrimSpace(request.AgentSystemPrompt) == "" || !utf8.ValidString(request.AgentSystemPrompt) ||
		len(request.AgentSystemPrompt) > maxAgentSystemPromptBytes || request.MaxSteps == 0 ||
		request.MaxSteps > maxPlanSteps || request.ModelName != plannerModelName ||
		!request.ExecutionConfigHash.Valid() || !validGenerationParams(request.GenerationParams) ||
		request.AllowedTools == nil || len(request.AllowedTools) > maxPlannerTools ||
		request.ToolCatalogSelector.AllowedTools == nil ||
		!slices.Equal(request.AllowedTools, request.ToolCatalogSelector.AllowedTools) {
		return plannerError(PlannerErrorRuntimeFatal, PlannerCauseRuntimeInvalidRequest, PlannerPhaseInitial, nil)
	}
	seen := make(map[string]struct{}, len(request.AllowedTools))
	for _, name := range request.AllowedTools {
		if strings.TrimSpace(name) == "" || !utf8.ValidString(name) {
			return plannerError(PlannerErrorRuntimeFatal, PlannerCauseRuntimeInvalidRequest, PlannerPhaseInitial, nil)
		}
		if _, exists := seen[name]; exists {
			return plannerError(PlannerErrorRuntimeFatal, PlannerCauseRuntimeInvalidRequest, PlannerPhaseInitial, nil)
		}
		seen[name] = struct{}{}
	}
	if len(request.TaskInput) > maxTaskInputBytes {
		return plannerError(PlannerErrorPlanGenerationFailed, PlannerCauseTaskInputTooLarge, PlannerPhaseInitial, nil)
	}
	return nil
}

func validGenerationParams(params contracts.GenerationParams) bool {
	temperature, ok := new(big.Rat).SetString(params.Temperature.String())
	if !ok || temperature.Sign() < 0 || temperature.Cmp(big.NewRat(2, 1)) > 0 {
		return false
	}
	topP, ok := new(big.Rat).SetString(params.TopP.String())
	return ok && topP.Sign() > 0 && topP.Cmp(big.NewRat(1, 1)) <= 0 &&
		params.MaxOutputTokens > 0 && params.MaxOutputTokens <= 8192
}

func (s *Service) generate(
	ctx context.Context,
	request PlannerRequest,
	messages []contracts.ModelMessage,
	phase PlannerPhase,
	timeout time.Duration,
) (contracts.ModelResponse, error) {
	if s == nil || s.model == nil {
		return contracts.ModelResponse{}, plannerError(
			PlannerErrorRuntimeFatal, PlannerCauseRuntimeContractBroken, phase, nil,
		)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := s.model.GenerateStructured(callCtx, contracts.ModelRequest{
		Model: plannerModelName, Stream: false, ResponseFormat: plannerResponseFormat,
		Messages: messages, GenerationParams: request.GenerationParams,
		Metadata: contracts.ModelRequestMetadata{
			Operation: "GeneratePlan", Phase: string(phase), TaskID: request.TaskID,
			RunID: request.RunID, ExecutionVersion: executionVersionPointer(request.ExecutionVersion),
		},
	})
	if canceled := plannerCancellation(ctx, phase); canceled != nil {
		return contracts.ModelResponse{}, canceled
	}
	if callCtx.Err() != nil {
		return contracts.ModelResponse{}, plannerError(
			PlannerErrorPlanGenerationFailed, PlannerCauseModelCallTimeout, phase, nil,
		)
	}
	if err != nil {
		if response.AssistantContent != "" || response.ProviderRequestID != nil {
			return contracts.ModelResponse{}, plannerError(
				PlannerErrorRuntimeFatal, PlannerCauseRuntimeContractBroken, phase, nil,
			)
		}
		return contracts.ModelResponse{}, mapModelError(err, phase)
	}
	if len(response.AssistantContent) > maxModelResponseBytes {
		return contracts.ModelResponse{}, plannerError(
			PlannerErrorPlanGenerationFailed, PlannerCauseModelResponseTooLarge, phase, nil,
		)
	}
	if strings.TrimSpace(response.AssistantContent) == "" || !utf8.ValidString(response.AssistantContent) {
		return contracts.ModelResponse{}, &repairableModelError{phase: phase}
	}
	return response, nil
}

func (s *Service) validateCandidate(
	content string,
	request PlannerRequest,
	snapshot contracts.PlanningToolSnapshot,
) (PlanDraft, []RepairIssue, bool) {
	draft, err := s.parser.ParseV1([]byte(content))
	if err != nil {
		var parseErr *ParseError
		if errors.As(err, &parseErr) && parseErr != nil {
			return PlanDraft{}, []RepairIssue{{Code: string(parseErr.Code), Path: parseErr.Path, Summary: string(parseErr.Code)}}, false
		}
		return PlanDraft{}, []RepairIssue{{Code: string(ParseIssueInvalidJSON), Path: "$", Summary: string(ParseIssueInvalidJSON)}}, false
	}
	validation := s.verify.Validate(ValidatePlanRequest{
		Draft: draft, MaxSteps: request.MaxSteps, AllowedTools: request.AllowedTools, ToolSnapshot: snapshot,
	})
	issues := make([]RepairIssue, len(validation))
	for index, issue := range validation {
		issues[index] = RepairIssue{Code: string(issue.Code), Path: issue.Path, Summary: issue.Summary}
	}
	return draft, issues, true
}

func (s *Service) repair(
	ctx context.Context,
	request PlannerRequest,
	snapshot contracts.PlanningToolSnapshot,
	initial InitialPromptRequest,
	candidate *PlanDraft,
	issues []RepairIssue,
) (ValidatedPlanDraft, error) {
	if canceled := plannerCancellation(ctx, PlannerPhaseRepair); canceled != nil {
		return ValidatedPlanDraft{}, canceled
	}
	timeout, ok := repairTimeout(ctx)
	if !ok {
		return ValidatedPlanDraft{}, plannerError(
			PlannerErrorPlanValidationFailed, PlannerCauseRepairBudgetInsufficient, PlannerPhaseRepair, issues,
		)
	}
	gate := NewSingleRepairGate(s.prompt)
	messages, normalized, err := gate.Build(RepairPromptRequest{
		InitialPromptRequest: initial, Candidate: candidate, Issues: issues,
	})
	if err != nil {
		var promptErr *PromptError
		if errors.As(err, &promptErr) && promptErr.Code == PromptErrorRepairPromptTooLarge {
			return ValidatedPlanDraft{}, plannerError(
				PlannerErrorPlanValidationFailed, PlannerCauseRepairPromptTooLarge, PlannerPhaseRepair, normalized,
			)
		}
		return ValidatedPlanDraft{}, plannerError(
			PlannerErrorRuntimeFatal, PlannerCauseRuntimePromptInvariantBroken, PlannerPhaseRepair, nil,
		)
	}
	response, modelErr := s.generate(ctx, request, messages, PlannerPhaseRepair, timeout)
	if modelErr != nil {
		if isRepairableModelError(modelErr) {
			return ValidatedPlanDraft{}, plannerError(
				PlannerErrorPlanValidationFailed, PlannerCauseRepairExhausted, PlannerPhaseRepair, normalized,
			)
		}
		return ValidatedPlanDraft{}, modelErr
	}
	draft, repairIssues, _ := s.validateCandidate(response.AssistantContent, request, snapshot)
	if len(repairIssues) != 0 {
		return ValidatedPlanDraft{}, plannerError(
			PlannerErrorPlanValidationFailed, PlannerCauseRepairExhausted, PlannerPhaseRepair, repairIssues,
		)
	}
	if canceled := plannerCancellation(ctx, PlannerPhaseRepair); canceled != nil {
		return ValidatedPlanDraft{}, canceled
	}
	return validatedResult(request, draft), nil
}

func repairTimeout(ctx context.Context) (time.Duration, bool) {
	deadline, exists := ctx.Deadline()
	if !exists {
		return plannerModelCallTimeout, true
	}
	remaining := time.Until(deadline)
	if remaining <= repairMinModelBudget+plannerLocalSafetyMargin {
		return 0, false
	}
	available := remaining - plannerLocalSafetyMargin
	return min(plannerModelCallTimeout, available), true
}

func (s *Service) callTimeout() time.Duration {
	if s == nil || s.modelCallTimeout <= 0 {
		return plannerModelCallTimeout
	}
	return s.modelCallTimeout
}

func mapCatalogError(err error) error {
	var typed *contracts.PlanningToolCatalogError
	if errors.As(err, &typed) && typed != nil {
		kind := typed.Kind
		return &PlannerError{
			Kind: PlannerErrorRuntimeFatal, CauseCode: PlannerCauseRuntimeStaticToolSnapshotInconsistent,
			Phase: PlannerPhaseInitial, CatalogKind: &kind,
		}
	}
	return plannerError(PlannerErrorRuntimeFatal, PlannerCauseRuntimeContractBroken, PlannerPhaseInitial, nil)
}

func mapModelError(err error, phase PlannerPhase) error {
	var typed *contracts.ModelClientError
	if !errors.As(err, &typed) || typed == nil || !typed.Kind.Valid() {
		return plannerError(PlannerErrorRuntimeFatal, PlannerCauseRuntimeContractBroken, phase, nil)
	}
	switch typed.Kind {
	case contracts.ModelClientErrorContractViolation:
		return plannerError(PlannerErrorRuntimeFatal, PlannerCauseRuntimeInvalidModelClientRequest, phase, nil)
	case contracts.ModelClientErrorCanceled:
		return plannerContextError(
			PlannerErrorCanceled, cancellationCode(err), phase, nil, adapterCancellationSentinel(err),
		)
	case contracts.ModelClientErrorTimeout:
		return plannerError(PlannerErrorPlanGenerationFailed, PlannerCauseModelProviderTimeout, phase, nil)
	case contracts.ModelClientErrorAuthentication:
		return plannerError(PlannerErrorPlanGenerationFailed, PlannerCauseModelAuthentication, phase, nil)
	case contracts.ModelClientErrorNetwork:
		return plannerError(PlannerErrorPlanGenerationFailed, PlannerCauseModelNetwork, phase, nil)
	case contracts.ModelClientErrorRateLimited:
		return plannerError(PlannerErrorPlanGenerationFailed, PlannerCauseModelRateLimited, phase, nil)
	case contracts.ModelClientErrorProvider:
		return plannerError(PlannerErrorPlanGenerationFailed, PlannerCauseModelProviderError, phase, nil)
	case contracts.ModelClientErrorResponseTooLarge:
		return plannerError(PlannerErrorPlanGenerationFailed, PlannerCauseModelResponseTooLarge, phase, nil)
	case contracts.ModelClientErrorInvalidResponse:
		return &repairableModelError{phase: phase}
	default:
		return plannerError(PlannerErrorRuntimeFatal, PlannerCauseRuntimeContractBroken, phase, nil)
	}
}

type repairableModelError struct{ phase PlannerPhase }

func (e *repairableModelError) Error() string {
	return "planner model response is unavailable for validation"
}

func isRepairableModelError(err error) bool {
	var target *repairableModelError
	return errors.As(err, &target)
}

func plannerCancellation(ctx context.Context, phase PlannerPhase) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	type codedCause interface{ PlannerCancellationCause() PlannerCauseCode }
	var coded codedCause
	if errors.As(cause, &coded) {
		code := coded.PlannerCancellationCause()
		switch code {
		case PlannerCauseTaskCancelled, PlannerCauseTaskTimedOut, PlannerCauseActionTimeout,
			PlannerCauseRuntimeShutdown, PlannerCauseLockLost:
			return plannerContextError(PlannerErrorCanceled, code, phase, nil, ctx.Err())
		}
	}
	return plannerContextError(
		PlannerErrorCanceled, fallbackCancellationCode(cause), phase, nil, ctx.Err(),
	)
}

func fallbackCancellationCode(err error) PlannerCauseCode {
	if errors.Is(err, context.DeadlineExceeded) {
		return PlannerCauseActionTimeout
	}
	return PlannerCauseRuntimeShutdown
}

func cancellationCode(err error) PlannerCauseCode {
	type codedCause interface{ PlannerCancellationCause() PlannerCauseCode }
	var coded codedCause
	if errors.As(err, &coded) {
		code := coded.PlannerCancellationCause()
		switch code {
		case PlannerCauseTaskCancelled, PlannerCauseTaskTimedOut, PlannerCauseActionTimeout,
			PlannerCauseRuntimeShutdown, PlannerCauseLockLost:
			return code
		}
	}
	return fallbackCancellationCode(err)
}

func adapterCancellationSentinel(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

func plannerError(kind PlannerErrorKind, code PlannerCauseCode, phase PlannerPhase, issues []RepairIssue) *PlannerError {
	return &PlannerError{Kind: kind, CauseCode: code, Phase: phase, Issues: slices.Clone(issues)}
}

func plannerContextError(
	kind PlannerErrorKind,
	code PlannerCauseCode,
	phase PlannerPhase,
	issues []RepairIssue,
	cause error,
) *PlannerError {
	err := plannerError(kind, code, phase, issues)
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		err.cause = context.DeadlineExceeded
	case errors.Is(cause, context.Canceled):
		err.cause = context.Canceled
	}
	return err
}

func executionVersionPointer(version contracts.ExecutionVersion) *contracts.ExecutionVersion {
	copy := version
	return &copy
}

func validatedResult(request PlannerRequest, draft PlanDraft) ValidatedPlanDraft {
	steps := make([]StepDraft, len(draft.Steps))
	copy(steps, draft.Steps)
	return ValidatedPlanDraft{
		TaskID: request.TaskID, RunID: request.RunID, ExecutionVersion: request.ExecutionVersion,
		Goal: draft.Goal, Steps: steps,
	}
}

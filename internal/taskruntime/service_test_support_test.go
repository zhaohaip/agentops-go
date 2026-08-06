package taskruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhaohaip/agentops-go/internal/config/business"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

type fakeStore struct {
	tasks               map[contracts.TaskID]taskruntime.Task
	runs                map[contracts.TaskID]taskruntime.Run
	executions          map[string]taskruntime.TaskExecution
	receipts            map[taskruntime.CommandID]taskruntime.CommandReceipt
	checkpoints         []taskruntime.RuntimeCheckpoint
	reports             []contracts.EnsurePendingReportRequest
	logs                []taskruntime.TaskLog
	terminationSteps    map[contracts.TaskID]taskruntime.TerminationStep
	terminationTools    map[contracts.TaskID]taskruntime.TerminationToolExecution
	startupFacts        []taskruntime.StartupCleanupFacts
	startupApplications []taskruntime.ApplyStartupCleanupRequest
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tasks: make(map[contracts.TaskID]taskruntime.Task), runs: make(map[contracts.TaskID]taskruntime.Run),
		executions:       make(map[string]taskruntime.TaskExecution),
		receipts:         make(map[taskruntime.CommandID]taskruntime.CommandReceipt),
		terminationSteps: make(map[contracts.TaskID]taskruntime.TerminationStep),
		terminationTools: make(map[contracts.TaskID]taskruntime.TerminationToolExecution),
	}
}

func (s *fakeStore) clone() *fakeStore {
	copyStore := newFakeStore()
	for key, value := range s.tasks {
		copyStore.tasks[key] = value
	}
	for key, value := range s.runs {
		value.Context = append(json.RawMessage(nil), value.Context...)
		copyStore.runs[key] = value
	}
	for key, value := range s.executions {
		copyStore.executions[key] = value
	}
	for key, value := range s.receipts {
		value.Response = append(json.RawMessage(nil), value.Response...)
		copyStore.receipts[key] = value
	}
	copyStore.checkpoints = append([]taskruntime.RuntimeCheckpoint(nil), s.checkpoints...)
	copyStore.reports = append([]contracts.EnsurePendingReportRequest(nil), s.reports...)
	copyStore.logs = append([]taskruntime.TaskLog(nil), s.logs...)
	for key, value := range s.terminationSteps {
		copyStore.terminationSteps[key] = value
	}
	for key, value := range s.terminationTools {
		copyStore.terminationTools[key] = value
	}
	copyStore.startupFacts = make([]taskruntime.StartupCleanupFacts, len(s.startupFacts))
	for index, facts := range s.startupFacts {
		copyStore.startupFacts[index] = facts
		if facts.Step != nil {
			step := *facts.Step
			copyStore.startupFacts[index].Step = &step
		}
		if facts.ToolExecution != nil {
			tool := *facts.ToolExecution
			copyStore.startupFacts[index].ToolExecution = &tool
		}
		if facts.ApprovedRecovery != nil {
			approval := *facts.ApprovedRecovery
			approval.FrozenToolInput = append(contracts.FrozenToolInput(nil), facts.ApprovedRecovery.FrozenToolInput...)
			approval.ObservedValues = append(contracts.ObservedValues(nil), facts.ApprovedRecovery.ObservedValues...)
			copyStore.startupFacts[index].ApprovedRecovery = &approval
		}
	}
	copyStore.startupApplications = append([]taskruntime.ApplyStartupCleanupRequest(nil), s.startupApplications...)
	return copyStore
}

type fakeWriteTx struct {
	store *fakeStore
}

func (*fakeWriteTx) AgentOpsRuntimeWriteTx() {}

type fakeExecutor struct {
	mu            sync.Mutex
	waiters       atomic.Int64
	waiterStarted chan struct{}
	store         *fakeStore
	commits       int
	rollbacks     int
	afterRollback func(*fakeStore)
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{store: newFakeStore()}
}

func (e *fakeExecutor) Execute(
	ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error,
) error {
	if !e.mu.TryLock() {
		e.waiters.Add(1)
		if e.waiterStarted != nil {
			select {
			case e.waiterStarted <- struct{}{}:
			default:
			}
		}
		e.mu.Lock()
		e.waiters.Add(-1)
	}
	return e.executeLocked(ctx, work)
}

func (e *fakeExecutor) TryExecute(
	ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error,
) (bool, error) {
	if e.waiters.Load() != 0 || !e.mu.TryLock() {
		return false, nil
	}
	if e.waiters.Load() != 0 {
		e.mu.Unlock()
		return false, nil
	}
	return true, e.executeLocked(ctx, work)
}

func (e *fakeExecutor) executeLocked(
	ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error,
) error {
	transactionStore := e.store.clone()
	if err := work(ctx, &fakeWriteTx{store: transactionStore}); err != nil {
		e.rollbacks++
		afterRollback := e.afterRollback
		e.afterRollback = nil
		committedStore := e.store
		e.mu.Unlock()
		if afterRollback != nil {
			afterRollback(committedStore)
		}
		return err
	}
	e.store = transactionStore
	e.commits++
	e.mu.Unlock()
	return nil
}

func (e *fakeExecutor) snapshot() *fakeStore {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.clone()
}

type fakeRepositories struct {
	executor      *fakeExecutor
	now           time.Time
	failOperation map[string]error
	missOperation map[string]bool
	mu            sync.Mutex
	seenTx        []contracts.RuntimeWriteTx
	operationTx   map[string][]contracts.RuntimeWriteTx
	logDeadline   bool
}

func newFakeRepositories(executor *fakeExecutor, now time.Time) *fakeRepositories {
	return &fakeRepositories{
		executor: executor, now: now, failOperation: make(map[string]error), missOperation: make(map[string]bool),
		operationTx: make(map[string][]contracts.RuntimeWriteTx),
	}
}

func (r *fakeRepositories) transaction(token contracts.RuntimeWriteTx, operation string) (*fakeWriteTx, error) {
	r.mu.Lock()
	r.seenTx = append(r.seenTx, token)
	r.operationTx[operation] = append(r.operationTx[operation], token)
	failure := r.failOperation[operation]
	miss := r.missOperation[operation]
	if miss {
		delete(r.missOperation, operation)
	}
	r.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	if miss {
		return nil, errFakeConditionalMiss
	}
	transaction, ok := token.(*fakeWriteTx)
	if !ok || transaction == nil || transaction.store == nil {
		return nil, errors.New("invalid fake transaction token")
	}
	return transaction, nil
}

var errFakeConditionalMiss = errors.New("fake conditional update missed")

func executionKey(taskID contracts.TaskID, version contracts.ExecutionVersion) string {
	return fmt.Sprintf("%s/%d", taskID, version)
}

func (r *fakeRepositories) Insert(ctx context.Context, tx contracts.RuntimeWriteTx, task taskruntime.Task) error {
	transaction, err := r.transaction(tx, "task.insert")
	if err != nil {
		return err
	}
	if _, exists := transaction.store.tasks[task.TaskID]; exists {
		return errors.New("duplicate Task")
	}
	transaction.store.tasks[task.TaskID] = task
	return nil
}

func (r *fakeRepositories) Find(_ context.Context, taskID contracts.TaskID) (taskruntime.Task, error) {
	store := r.executor.snapshot()
	task, exists := store.tasks[taskID]
	if !exists {
		return taskruntime.Task{}, taskruntime.ErrRepositoryNotFound
	}
	return task, nil
}

func (r *fakeRepositories) Lock(_ context.Context, tx contracts.RuntimeWriteTx, taskID contracts.TaskID) (taskruntime.Task, error) {
	transaction, err := r.transaction(tx, "task.lock")
	if err != nil {
		return taskruntime.Task{}, err
	}
	task, exists := transaction.store.tasks[taskID]
	if !exists {
		return taskruntime.Task{}, taskruntime.ErrRepositoryNotFound
	}
	return task, nil
}

func (r *fakeRepositories) LockNextQueueCandidate(
	_ context.Context,
	tx contracts.RuntimeWriteTx,
) (taskruntime.QueueCandidate, error) {
	transaction, err := r.transaction(tx, "task.lock_next")
	if err != nil {
		return taskruntime.QueueCandidate{}, err
	}
	candidates := make([]taskruntime.QueueCandidate, 0)
	for _, task := range transaction.store.tasks {
		if task.QueuedAt == nil {
			continue
		}
		execution, exists := transaction.store.executions[executionKey(task.TaskID, task.CurrentExecutionVersion)]
		if !exists {
			continue
		}
		candidates = append(candidates, taskruntime.QueueCandidate{
			TaskID: task.TaskID, RunID: task.CurrentRunID, ExecutionVersion: task.CurrentExecutionVersion,
			TaskStatus: task.Status, ExecutionStatus: execution.Status,
			QueuedAt: *task.QueuedAt, CreatedAt: task.CreatedAt,
		})
	}
	if len(candidates) == 0 {
		return taskruntime.QueueCandidate{}, taskruntime.ErrRepositoryNotFound
	}
	sort.Slice(candidates, func(left, right int) bool {
		if !candidates[left].QueuedAt.Equal(candidates[right].QueuedAt) {
			return candidates[left].QueuedAt.Before(candidates[right].QueuedAt)
		}
		if !candidates[left].CreatedAt.Equal(candidates[right].CreatedAt) {
			return candidates[left].CreatedAt.Before(candidates[right].CreatedAt)
		}
		return candidates[left].TaskID < candidates[right].TaskID
	})
	return candidates[0], nil
}

func (r *fakeRepositories) Update(
	_ context.Context,
	tx contracts.RuntimeWriteTx,
	update taskruntime.TaskUpdate,
) (bool, error) {
	transaction, err := r.transaction(tx, "task.update")
	if errors.Is(err, errFakeConditionalMiss) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	task, exists := transaction.store.tasks[update.TaskID]
	if !exists || task.Status != update.ExpectedStatus ||
		task.CurrentExecutionVersion != update.ExpectedCurrentExecutionVersion {
		return false, nil
	}
	task.Status = update.Status
	task.CurrentExecutionVersion = update.CurrentExecutionVersion
	task.ResultSummary = update.ResultSummary
	task.ErrorCode = update.ErrorCode
	task.QueuedAt = update.QueuedAt
	task.StartedAt = update.StartedAt
	task.EndedAt = update.EndedAt
	transaction.store.tasks[task.TaskID] = task
	return true, nil
}

func (r *fakeRepositories) InsertRun(_ context.Context, tx contracts.RuntimeWriteTx, run taskruntime.Run) error {
	transaction, err := r.transaction(tx, "run.insert")
	if err != nil {
		return err
	}
	if _, exists := transaction.store.runs[run.TaskID]; exists {
		return errors.New("duplicate Run")
	}
	transaction.store.runs[run.TaskID] = run
	return nil
}

// Go 不支持按参数类型重载；以下小包装器由专用 Repository 适配类型调用。
type fakeTaskRepository struct{ repositories *fakeRepositories }
type fakeRunRepository struct{ repositories *fakeRepositories }
type fakeExecutionRepository struct{ repositories *fakeRepositories }
type fakeReceiptRepository struct{ repositories *fakeRepositories }
type fakeTaskLogRepository struct{ repositories *fakeRepositories }

func (r fakeTaskRepository) Insert(ctx context.Context, tx contracts.RuntimeWriteTx, task taskruntime.Task) error {
	return r.repositories.Insert(ctx, tx, task)
}
func (r fakeTaskRepository) Find(ctx context.Context, id contracts.TaskID) (taskruntime.Task, error) {
	return r.repositories.Find(ctx, id)
}
func (r fakeTaskRepository) Lock(ctx context.Context, tx contracts.RuntimeWriteTx, id contracts.TaskID) (taskruntime.Task, error) {
	return r.repositories.Lock(ctx, tx, id)
}
func (r fakeTaskRepository) LockNextQueueCandidate(ctx context.Context, tx contracts.RuntimeWriteTx) (taskruntime.QueueCandidate, error) {
	return r.repositories.LockNextQueueCandidate(ctx, tx)
}
func (r fakeTaskRepository) Update(ctx context.Context, tx contracts.RuntimeWriteTx, update taskruntime.TaskUpdate) (bool, error) {
	return r.repositories.Update(ctx, tx, update)
}

func (r fakeRunRepository) Insert(ctx context.Context, tx contracts.RuntimeWriteTx, run taskruntime.Run) error {
	return r.repositories.InsertRun(ctx, tx, run)
}
func (r fakeRunRepository) FindByTask(_ context.Context, taskID contracts.TaskID) (taskruntime.Run, error) {
	store := r.repositories.executor.snapshot()
	run, exists := store.runs[taskID]
	if !exists {
		return taskruntime.Run{}, taskruntime.ErrRepositoryNotFound
	}
	return run, nil
}
func (r fakeRunRepository) LockByTask(_ context.Context, tx contracts.RuntimeWriteTx, taskID contracts.TaskID) (taskruntime.Run, error) {
	transaction, err := r.repositories.transaction(tx, "run.lock")
	if err != nil {
		return taskruntime.Run{}, err
	}
	run, exists := transaction.store.runs[taskID]
	if !exists {
		return taskruntime.Run{}, taskruntime.ErrRepositoryNotFound
	}
	return run, nil
}
func (r fakeRunRepository) Update(_ context.Context, tx contracts.RuntimeWriteTx, update taskruntime.RunUpdate) (bool, error) {
	transaction, err := r.repositories.transaction(tx, "run.update")
	if errors.Is(err, errFakeConditionalMiss) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	run, exists := transaction.store.runs[update.TaskID]
	task, taskExists := transaction.store.tasks[update.TaskID]
	if !exists || !taskExists || run.RunID != update.RunID || run.Status != update.ExpectedStatus ||
		task.CurrentExecutionVersion != update.ExecutionVersion || task.CurrentRunID != update.RunID {
		return false, nil
	}
	run.Status = update.Status
	run.PlanID = update.PlanID
	run.CurrentStepID = update.CurrentStepID
	run.Context = append(json.RawMessage(nil), update.Context...)
	run.ErrorCode = update.ErrorCode
	run.StartedAt = update.StartedAt
	run.EndedAt = update.EndedAt
	transaction.store.runs[update.TaskID] = run
	return true, nil
}

func (r fakeExecutionRepository) Insert(_ context.Context, tx contracts.RuntimeWriteTx, execution taskruntime.TaskExecution) error {
	transaction, err := r.repositories.transaction(tx, "execution.insert")
	if err != nil {
		return err
	}
	key := executionKey(execution.TaskID, execution.ExecutionVersion)
	if _, exists := transaction.store.executions[key]; exists {
		return errors.New("duplicate TaskExecution")
	}
	transaction.store.executions[key] = execution
	return nil
}
func (r fakeExecutionRepository) FindByTaskVersion(
	_ context.Context,
	taskID contracts.TaskID,
	version contracts.ExecutionVersion,
) (taskruntime.TaskExecution, error) {
	store := r.repositories.executor.snapshot()
	execution, exists := store.executions[executionKey(taskID, version)]
	if !exists {
		return taskruntime.TaskExecution{}, taskruntime.ErrRepositoryNotFound
	}
	return execution, nil
}
func (r fakeExecutionRepository) LockByTaskVersion(
	_ context.Context,
	tx contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
	version contracts.ExecutionVersion,
) (taskruntime.TaskExecution, error) {
	transaction, err := r.repositories.transaction(tx, "execution.lock")
	if err != nil {
		return taskruntime.TaskExecution{}, err
	}
	execution, exists := transaction.store.executions[executionKey(taskID, version)]
	if !exists {
		return taskruntime.TaskExecution{}, taskruntime.ErrRepositoryNotFound
	}
	return execution, nil
}
func (r fakeExecutionRepository) Update(
	_ context.Context,
	tx contracts.RuntimeWriteTx,
	update taskruntime.TaskExecutionUpdate,
) (bool, error) {
	transaction, err := r.repositories.transaction(tx, "execution.update")
	if errors.Is(err, errFakeConditionalMiss) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	task, taskExists := transaction.store.tasks[update.TaskID]
	key := executionKey(update.TaskID, update.ExecutionVersion)
	execution, exists := transaction.store.executions[key]
	if !taskExists || !exists || task.CurrentExecutionVersion != update.ExecutionVersion ||
		execution.Status != update.ExpectedStatus || !equalWorkerID(execution.WorkerID, update.ExpectedWorkerID) {
		return false, nil
	}
	if update.ObservedConfigHash != nil && execution.ObservedConfigHash != nil {
		return false, nil
	}
	execution.Status = update.Status
	execution.WorkerID = update.WorkerID
	if update.ObservedConfigHash != nil {
		execution.ObservedConfigHash = update.ObservedConfigHash
	}
	execution.ErrorCode = update.ErrorCode
	execution.InvariantCode = update.InvariantCode
	execution.TerminationReason = update.TerminationReason
	execution.StartedAt = update.StartedAt
	execution.EndedAt = update.EndedAt
	transaction.store.executions[key] = execution
	return true, nil
}

func equalWorkerID(left, right *contracts.WorkerID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r fakeReceiptRepository) Insert(_ context.Context, tx contracts.RuntimeWriteTx, receipt taskruntime.CommandReceipt) error {
	transaction, err := r.repositories.transaction(tx, "receipt.insert")
	if err != nil {
		return err
	}
	if _, exists := transaction.store.receipts[receipt.CommandID]; exists {
		return errors.New("duplicate Receipt")
	}
	transaction.store.receipts[receipt.CommandID] = receipt
	return nil
}
func (r fakeReceiptRepository) Find(_ context.Context, commandID taskruntime.CommandID) (taskruntime.CommandReceipt, error) {
	store := r.repositories.executor.snapshot()
	receipt, exists := store.receipts[commandID]
	if !exists {
		return taskruntime.CommandReceipt{}, taskruntime.ErrRepositoryNotFound
	}
	return receipt, nil
}
func (r fakeReceiptRepository) Lock(
	_ context.Context,
	tx contracts.RuntimeWriteTx,
	commandID taskruntime.CommandID,
) (taskruntime.CommandReceipt, error) {
	transaction, err := r.repositories.transaction(tx, "receipt.lock")
	if err != nil {
		return taskruntime.CommandReceipt{}, err
	}
	receipt, exists := transaction.store.receipts[commandID]
	if !exists {
		return taskruntime.CommandReceipt{}, taskruntime.ErrRepositoryNotFound
	}
	return receipt, nil
}

func (r *fakeRepositories) Now(_ context.Context, tx contracts.RuntimeWriteTx) (time.Time, error) {
	if _, err := r.transaction(tx, "clock.now"); err != nil {
		return time.Time{}, err
	}
	return r.now, nil
}

func (r fakeTaskLogRepository) Append(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	log taskruntime.TaskLog,
) error {
	_, hasDeadline := ctx.Deadline()
	r.repositories.mu.Lock()
	r.repositories.logDeadline = hasDeadline
	r.repositories.mu.Unlock()
	transaction, err := r.repositories.transaction(tx, "task_log.append")
	if err != nil {
		return err
	}
	transaction.store.logs = append(transaction.store.logs, log)
	return nil
}

type fakeAgentConfigSource struct {
	agents map[contracts.AgentID]taskruntime.AgentRuntimeConfig
	calls  int
}

func (s *fakeAgentConfigSource) LookupAgent(agentID contracts.AgentID) (taskruntime.AgentRuntimeConfig, bool) {
	s.calls++
	agent, exists := s.agents[agentID]
	return agent, exists
}

type fakeCheckpointPort struct {
	overrides        map[contracts.TaskID]taskruntime.ClaimCheckpointResult
	startupOverrides map[contracts.TaskID]taskruntime.StartupCleanupCheckpointResult
	failSave         error
	failLoad         error
	failStartup      error
	mu               sync.Mutex
	seenTx           []contracts.RuntimeWriteTx
}

func (p *fakeCheckpointPort) LoadLatestForStartupCleanup(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
	_ contracts.RunID,
	_ contracts.ExecutionVersion,
) (taskruntime.StartupCleanupCheckpointResult, error) {
	p.mu.Lock()
	p.seenTx = append(p.seenTx, token)
	p.mu.Unlock()
	if p.failStartup != nil {
		return nil, p.failStartup
	}
	if result, exists := p.startupOverrides[taskID]; exists {
		return result, nil
	}
	return taskruntime.StartupCleanupCheckpointInvalid{ReasonCode: contracts.ReasonCodeCheckpointNotFound}, nil
}

func (p *fakeCheckpointPort) SaveRuntimeCheckpoint(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	request taskruntime.SaveRuntimeCheckpointRequest,
) error {
	p.mu.Lock()
	p.seenTx = append(p.seenTx, token)
	p.mu.Unlock()
	if p.failSave != nil {
		return p.failSave
	}
	transaction, ok := token.(*fakeWriteTx)
	if !ok || transaction == nil || transaction.store == nil {
		return errors.New("invalid fake checkpoint transaction")
	}
	sequence := int64(1)
	for _, checkpoint := range transaction.store.checkpoints {
		if checkpoint.RunID == request.RunID && checkpoint.CheckpointSequence >= sequence {
			sequence = checkpoint.CheckpointSequence + 1
		}
	}
	transaction.store.checkpoints = append(transaction.store.checkpoints, taskruntime.RuntimeCheckpoint{
		CheckpointID: contracts.CheckpointID(fmt.Sprintf("checkpoint-%d", len(transaction.store.checkpoints)+1)),
		TaskID:       request.TaskID, RunID: request.RunID, ExecutionVersion: request.ExecutionVersion,
		ExecutionConfigHash: request.ExecutionConfigHash, NextAction: request.NextAction,
		CheckpointSequence: sequence,
	})
	return nil
}

func (p *fakeCheckpointPort) LoadLatestForClaim(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
	runID contracts.RunID,
	version contracts.ExecutionVersion,
	_ taskruntime.ClaimCheckpointSource,
) (taskruntime.ClaimCheckpointResult, error) {
	p.mu.Lock()
	p.seenTx = append(p.seenTx, token)
	p.mu.Unlock()
	if p.failLoad != nil {
		return nil, p.failLoad
	}
	if result, exists := p.overrides[taskID]; exists {
		return result, nil
	}
	transaction, ok := token.(*fakeWriteTx)
	if !ok || transaction == nil || transaction.store == nil {
		return nil, errors.New("invalid fake checkpoint transaction")
	}
	var latest *taskruntime.RuntimeCheckpoint
	for index := range transaction.store.checkpoints {
		checkpoint := transaction.store.checkpoints[index]
		if checkpoint.TaskID == taskID && checkpoint.RunID == runID && checkpoint.ExecutionVersion == version &&
			(latest == nil || checkpoint.CheckpointSequence > latest.CheckpointSequence) {
			copyCheckpoint := checkpoint
			latest = &copyCheckpoint
		}
	}
	if latest == nil {
		return taskruntime.ClaimCheckpointInvalid{ReasonCode: contracts.ReasonCodeCheckpointNotFound}, nil
	}
	return taskruntime.ClaimCheckpointValid{Checkpoint: *latest}, nil
}

func (p *fakeCheckpointPort) LoadLatestForExecutionDispatch(
	ctx context.Context,
	token contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
	runID contracts.RunID,
	version contracts.ExecutionVersion,
) (taskruntime.ExecutionCheckpointResult, error) {
	result, err := p.LoadLatestForClaim(
		ctx, token, taskID, runID, version, taskruntime.ClaimCheckpointSourceContinuation,
	)
	if err != nil {
		return nil, err
	}
	switch value := result.(type) {
	case taskruntime.ClaimCheckpointValid:
		return taskruntime.ExecutionCheckpointValid{Checkpoint: value.Checkpoint}, nil
	case taskruntime.ClaimCheckpointInvalid:
		return taskruntime.ExecutionCheckpointInvalid{ReasonCode: value.ReasonCode}, nil
	default:
		return nil, errors.New("unknown fake Claim Checkpoint result")
	}
}

type fakePendingReportWriter struct {
	fail   error
	mu     sync.Mutex
	seenTx []contracts.RuntimeWriteTx
}

type fakeTerminationRepository struct {
	repositories *fakeRepositories
	applyHook    func()
}

type fakeStartupCleanupRepository struct {
	repositories *fakeRepositories
}

func (r *fakeStartupCleanupRepository) LockLegacyRunningExecutions(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	currentWorkerID contracts.WorkerID,
) ([]taskruntime.StartupCleanupFacts, error) {
	transaction, err := r.repositories.transaction(token, "startup_cleanup.lock")
	if err != nil {
		return nil, err
	}
	result := make([]taskruntime.StartupCleanupFacts, 0, len(transaction.store.startupFacts))
	for _, facts := range transaction.store.startupFacts {
		if facts.Execution.Status != contracts.TaskExecutionStatusRunning {
			continue
		}
		if facts.Execution.WorkerID != nil && *facts.Execution.WorkerID == currentWorkerID {
			continue
		}
		result = append(result, facts)
	}
	return result, nil
}

func (r *fakeStartupCleanupRepository) ApplyStartupCleanup(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	request taskruntime.ApplyStartupCleanupRequest,
) (bool, error) {
	transaction, err := r.repositories.transaction(token, "startup_cleanup.apply")
	if errors.Is(err, errFakeConditionalMiss) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for index := range transaction.store.startupFacts {
		facts := &transaction.store.startupFacts[index]
		if facts.Task.TaskID != request.TaskID || facts.Execution.ExecutionVersion != request.ExecutionVersion {
			continue
		}
		if facts.Task.CurrentExecutionVersion != request.ExecutionVersion ||
			facts.Task.Status != request.ExpectedTaskStatus || facts.Run.Status != request.ExpectedRunStatus ||
			facts.Execution.Status != request.ExpectedExecutionStatus || facts.Execution.WorkerID == nil ||
			*facts.Execution.WorkerID != request.ExpectedWorkerID || facts.Task.QueuedAt != nil {
			return false, nil
		}
		if request.ExpectedStepStatus != nil && (facts.Step == nil || facts.Step.Status != *request.ExpectedStepStatus) {
			return false, nil
		}
		if request.ExpectedToolStatus != nil &&
			(facts.ToolExecution == nil || facts.ToolExecution.Status != *request.ExpectedToolStatus) {
			return false, nil
		}
		if request.ToolStatus != nil {
			if facts.ToolExecution == nil {
				return false, nil
			}
			facts.ToolExecution.Status = *request.ToolStatus
			facts.ToolExecution.ErrorCode = request.ToolErrorCode
			facts.ToolExecution.SideEffectUnknown = request.ToolSideEffectUnknown
			facts.ToolExecution.EndedAt = &request.EndedAt
		}
		facts.Execution.ErrorCode = nil
		if request.ExecutionErrorCode != "" {
			errorCode := request.ExecutionErrorCode
			facts.Execution.ErrorCode = &errorCode
		}
		facts.Execution.EndedAt = &request.EndedAt
		if request.Disposition == taskruntime.StartupCleanupInterrupt {
			facts.Execution.Status = contracts.TaskExecutionStatusInterrupted
		} else if request.Disposition == taskruntime.StartupCleanupTerminal {
			facts.Execution.Status = contracts.TaskExecutionStatusFailed
			facts.Execution.TerminationReason = request.TerminationReason
			facts.Task.Status = contracts.TaskStatusFailed
			facts.Task.ErrorCode = request.TaskErrorCode
			facts.Task.EndedAt = &request.EndedAt
			facts.Run.Status = contracts.RunStatusFailed
			facts.Run.ErrorCode = request.TaskErrorCode
			facts.Run.EndedAt = &request.EndedAt
			if facts.Step != nil {
				facts.Step.Status = contracts.StepStatusFailed
				facts.Step.ErrorCode = request.StepErrorCode
				facts.Step.EndedAt = &request.EndedAt
			}
		} else {
			return false, errors.New("unknown fake StartupCleanup disposition")
		}
		transaction.store.startupApplications = append(transaction.store.startupApplications, request)
		return true, nil
	}
	return false, nil
}

func (r *fakeTerminationRepository) LockTerminationFacts(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
) (taskruntime.TerminationFacts, error) {
	transaction, err := r.repositories.transaction(token, "termination.lock")
	if err != nil {
		return taskruntime.TerminationFacts{}, err
	}
	task, exists := transaction.store.tasks[taskID]
	if !exists {
		return taskruntime.TerminationFacts{}, taskruntime.ErrRepositoryNotFound
	}
	run, exists := transaction.store.runs[taskID]
	if !exists {
		return taskruntime.TerminationFacts{}, taskruntime.ErrRepositoryNotFound
	}
	execution, exists := transaction.store.executions[executionKey(taskID, task.CurrentExecutionVersion)]
	if !exists {
		return taskruntime.TerminationFacts{}, taskruntime.ErrRepositoryNotFound
	}
	facts := taskruntime.TerminationFacts{Task: task, Run: run, Execution: execution}
	if step, exists := transaction.store.terminationSteps[taskID]; exists {
		stepCopy := step
		facts.Step = &stepCopy
	}
	if tool, exists := transaction.store.terminationTools[taskID]; exists {
		toolCopy := tool
		facts.ToolExecution = &toolCopy
	}
	return facts, nil
}

func (r *fakeTerminationRepository) ApplyTermination(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	request taskruntime.ApplyTerminationRequest,
) (bool, error) {
	transaction, err := r.repositories.transaction(token, "termination.apply")
	if errors.Is(err, errFakeConditionalMiss) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.applyHook != nil {
		r.applyHook()
	}
	task, taskExists := transaction.store.tasks[request.TaskID]
	run, runExists := transaction.store.runs[request.TaskID]
	executionKey := executionKey(request.TaskID, request.ExpectedExecutionVersion)
	execution, executionExists := transaction.store.executions[executionKey]
	if !taskExists || !runExists || !executionExists ||
		task.CurrentExecutionVersion != request.ExpectedExecutionVersion ||
		task.Status != request.ExpectedTaskStatus || run.Status != request.ExpectedRunStatus ||
		execution.Status != request.ExpectedExecutionStatus {
		return false, nil
	}
	if request.ExpectedStepStatus != nil {
		step, exists := transaction.store.terminationSteps[request.TaskID]
		if !exists || step.Status != *request.ExpectedStepStatus {
			return false, nil
		}
		step.Status = contracts.StepStatusFailed
		step.ErrorCode = &request.StepErrorCode
		step.EndedAt = &request.EndedAt
		transaction.store.terminationSteps[request.TaskID] = step
	}
	if request.ExpectedToolStatus != nil {
		tool, exists := transaction.store.terminationTools[request.TaskID]
		if !exists || tool.Status != *request.ExpectedToolStatus || request.ToolStatus == nil {
			return false, nil
		}
		tool.Status = *request.ToolStatus
		tool.ErrorCode = request.ToolErrorCode
		tool.SideEffectUnknown = request.ToolSideEffectUnknown
		tool.EndedAt = &request.EndedAt
		transaction.store.terminationTools[request.TaskID] = tool
	}
	task.Status = request.TaskStatus
	task.ErrorCode = &request.TaskErrorCode
	task.QueuedAt = nil
	task.EndedAt = &request.EndedAt
	transaction.store.tasks[request.TaskID] = task
	run.Status = contracts.RunStatusFailed
	run.ErrorCode = &request.RunErrorCode
	run.EndedAt = &request.EndedAt
	transaction.store.runs[request.TaskID] = run
	execution.Status = contracts.TaskExecutionStatusFailed
	execution.ErrorCode = request.ExecutionErrorCode
	execution.TerminationReason = &request.TerminationReason
	if !request.PreserveExecutionEndedAt {
		execution.EndedAt = &request.EndedAt
	}
	transaction.store.executions[executionKey] = execution
	return true, nil
}

func (w *fakePendingReportWriter) EnsurePending(
	_ context.Context,
	token contracts.RuntimeWriteTx,
	request contracts.EnsurePendingReportRequest,
) (contracts.EnsurePendingReportResult, error) {
	w.mu.Lock()
	w.seenTx = append(w.seenTx, token)
	w.mu.Unlock()
	if w.fail != nil {
		return nil, w.fail
	}
	transaction, ok := token.(*fakeWriteTx)
	if !ok || transaction == nil || transaction.store == nil {
		return nil, errors.New("invalid fake report transaction")
	}
	for _, existing := range transaction.store.reports {
		if existing.TaskID == request.TaskID {
			return contracts.EnsurePendingReportExisting{}, nil
		}
	}
	transaction.store.reports = append(transaction.store.reports, request)
	return contracts.EnsurePendingReportCreated{}, nil
}

func loadedAgentConfig(t interface {
	Helper()
	Fatalf(string, ...any)
}) taskruntime.AgentRuntimeConfig {
	t.Helper()
	config, err := business.Load("../../configs/business.json")
	if err != nil {
		t.Fatalf("load business config: %v", err)
	}
	agent, exists := config.Lookup("agent-default")
	if !exists {
		t.Fatalf("agent-default missing")
	}
	allowedTools := make([]string, len(agent.ExecutionConfig.Agent.AllowedTools))
	for index, tool := range agent.ExecutionConfig.Agent.AllowedTools {
		allowedTools[index] = string(tool)
	}
	return taskruntime.AgentRuntimeConfig{
		TaskTimeout: agent.TaskTimeout, ExecutionConfig: agent.ExecutionConfig,
		PlanningToolCatalogSelector: contracts.PlanningToolCatalogSelector{
			CatalogID: agent.CatalogID, AllowedTools: allowedTools, ExpectedRegistryVersion: "registry-v1",
			ExpectedSnapshotHash: contracts.CatalogSnapshotHash(strings.Repeat("a", 64)),
		},
	}
}

func fakeRepositoryPorts(repositories *fakeRepositories) (
	taskruntime.TaskRepository,
	taskruntime.RunRepository,
	taskruntime.TaskExecutionRepository,
	taskruntime.CommandReceiptRepository,
) {
	return fakeTaskRepository{repositories}, fakeRunRepository{repositories},
		fakeExecutionRepository{repositories}, fakeReceiptRepository{repositories}
}

var (
	_ contracts.RuntimeWriteExecutor           = (*fakeExecutor)(nil)
	_ taskruntime.DatabaseClock                = (*fakeRepositories)(nil)
	_ taskruntime.AgentConfigSource            = (*fakeAgentConfigSource)(nil)
	_ taskruntime.RuntimeCheckpointPort        = (*fakeCheckpointPort)(nil)
	_ contracts.PendingReportWriter            = (*fakePendingReportWriter)(nil)
	_ taskruntime.TaskRepository               = fakeTaskRepository{}
	_ taskruntime.RunRepository                = fakeRunRepository{}
	_ taskruntime.TaskExecutionRepository      = fakeExecutionRepository{}
	_ taskruntime.CommandReceiptRepository     = fakeReceiptRepository{}
	_ taskruntime.TaskLogRepository            = fakeTaskLogRepository{}
	_ taskruntime.TerminationRepository        = (*fakeTerminationRepository)(nil)
	_ taskruntime.StartupCleanupRepository     = (*fakeStartupCleanupRepository)(nil)
	_ taskruntime.StartupCleanupCheckpointPort = (*fakeCheckpointPort)(nil)
)

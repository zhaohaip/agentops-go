package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/config/business"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

type phase1Store struct {
	tasks       map[contracts.TaskID]domain.Task
	runs        map[contracts.TaskID]domain.Run
	executions  map[string]domain.TaskExecution
	receipts    map[domain.CommandID]domain.CommandReceipt
	checkpoints []domain.RuntimeCheckpoint
	plans       map[contracts.TaskID]domain.ExecutionPlan
	steps       map[contracts.TaskID]map[contracts.StepID]domain.ExecutionStep
	logs        []domain.TaskLog
	reports     []contracts.EnsurePendingReportRequest
}

func newPhase1Store() *phase1Store {
	return &phase1Store{
		tasks: make(map[contracts.TaskID]domain.Task), runs: make(map[contracts.TaskID]domain.Run),
		executions: make(map[string]domain.TaskExecution), receipts: make(map[domain.CommandID]domain.CommandReceipt),
		plans: make(map[contracts.TaskID]domain.ExecutionPlan), steps: make(map[contracts.TaskID]map[contracts.StepID]domain.ExecutionStep),
	}
}

func (s *phase1Store) clone() *phase1Store {
	copyStore := newPhase1Store()
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
	copyStore.checkpoints = append([]domain.RuntimeCheckpoint(nil), s.checkpoints...)
	for key, value := range s.plans {
		copyStore.plans[key] = value
	}
	for taskID, steps := range s.steps {
		copyStore.steps[taskID] = make(map[contracts.StepID]domain.ExecutionStep, len(steps))
		for stepID, step := range steps {
			step.Input = append(json.RawMessage(nil), step.Input...)
			copyStore.steps[taskID][stepID] = step
		}
	}
	copyStore.logs = append([]domain.TaskLog(nil), s.logs...)
	copyStore.reports = append([]contracts.EnsurePendingReportRequest(nil), s.reports...)
	return copyStore
}

func (s *phase1Store) rollupReports() int { return len(s.reports) }

type phase1Tx struct{ store *phase1Store }

func (*phase1Tx) AgentOpsRuntimeWriteTx() {}

type phase1Executor struct {
	mu            sync.Mutex
	waiters       atomic.Int64
	waiterStarted chan struct{}
	store         *phase1Store
	commits       atomic.Int64
	rollbacks     atomic.Int64
}

func newPhase1Executor() *phase1Executor {
	return &phase1Executor{store: newPhase1Store(), waiterStarted: make(chan struct{}, 8)}
}

func (e *phase1Executor) Execute(ctx context.Context, work func(context.Context, contracts.RuntimeWriteTx) error) error {
	if !e.mu.TryLock() {
		e.waiters.Add(1)
		select {
		case e.waiterStarted <- struct{}{}:
		default:
		}
		e.mu.Lock()
		e.waiters.Add(-1)
	}
	return e.executeLocked(ctx, work)
}

func (e *phase1Executor) TryExecute(ctx context.Context, work func(context.Context, contracts.RuntimeWriteTx) error) (bool, error) {
	if e.waiters.Load() != 0 || !e.mu.TryLock() {
		return false, nil
	}
	if e.waiters.Load() != 0 {
		e.mu.Unlock()
		return false, nil
	}
	return true, e.executeLocked(ctx, work)
}

func (e *phase1Executor) executeLocked(ctx context.Context, work func(context.Context, contracts.RuntimeWriteTx) error) error {
	transaction := &phase1Tx{store: e.store.clone()}
	if err := work(ctx, transaction); err != nil {
		e.rollbacks.Add(1)
		e.mu.Unlock()
		return err
	}
	e.store = transaction.store
	e.commits.Add(1)
	e.mu.Unlock()
	return nil
}

func (e *phase1Executor) snapshot() *phase1Store {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.clone()
}

func (e *phase1Executor) awaitWaiter(t *testing.T) {
	t.Helper()
	select {
	case <-e.waiterStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("competing domain write did not reach the serialized gate")
	}
}

func strictStore(token contracts.RuntimeWriteTx) (*phase1Store, error) {
	tx, ok := token.(*phase1Tx)
	if !ok || tx == nil || tx.store == nil {
		return nil, errors.New("strict fake rejected foreign RuntimeWriteTx")
	}
	return tx.store, nil
}

type operationBarrier struct {
	entered  chan struct{}
	releaseC chan struct{}
}

func newOperationBarrier() *operationBarrier {
	return &operationBarrier{entered: make(chan struct{}), releaseC: make(chan struct{})}
}

func (b *operationBarrier) wait(ctx context.Context) error {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.releaseC:
		return nil
	}
}

func (b *operationBarrier) awaitEntered(t *testing.T) {
	t.Helper()
	awaitClosed(t, b.entered, "operation did not reach synchronization barrier")
}

func (b *operationBarrier) release() {
	select {
	case <-b.releaseC:
	default:
		close(b.releaseC)
	}
}

type phase1Clock struct {
	mu   sync.Mutex
	now  time.Time
	tick time.Duration
}

func (c *phase1Clock) Now(_ context.Context, token contracts.RuntimeWriteTx) (time.Time, error) {
	if _, err := strictStore(token); err != nil {
		return time.Time{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	c.now = c.now.Add(c.tick)
	return now, nil
}

func (c *phase1Clock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func (c *phase1Clock) peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

type phase1Repositories struct {
	executor           *phase1Executor
	claimBarrier       *operationBarrier
	terminationBarrier *operationBarrier
}

func (r *phase1Repositories) Insert(_ context.Context, token contracts.RuntimeWriteTx, task domain.Task) error {
	store, err := strictStore(token)
	if err != nil {
		return err
	}
	if _, exists := store.tasks[task.TaskID]; exists {
		return errors.New("duplicate Task")
	}
	store.tasks[task.TaskID] = task
	return nil
}

func (r *phase1Repositories) Find(_ context.Context, taskID contracts.TaskID) (domain.Task, error) {
	task, ok := r.executor.snapshot().tasks[taskID]
	if !ok {
		return domain.Task{}, domain.ErrRepositoryNotFound
	}
	return task, nil
}

func (r *phase1Repositories) List(_ context.Context, status *contracts.TaskStatus) ([]domain.Task, error) {
	store := r.executor.snapshot()
	result := make([]domain.Task, 0, len(store.tasks))
	for _, task := range store.tasks {
		if status == nil || task.Status == *status {
			result = append(result, task)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].TaskID < result[j].TaskID
	})
	return result, nil
}

func (r *phase1Repositories) Lock(_ context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID) (domain.Task, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.Task{}, err
	}
	task, ok := store.tasks[taskID]
	if !ok {
		return domain.Task{}, domain.ErrRepositoryNotFound
	}
	return task, nil
}

func (r *phase1Repositories) LockNextQueueCandidate(ctx context.Context, token contracts.RuntimeWriteTx) (domain.QueueCandidate, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.QueueCandidate{}, err
	}
	if r.claimBarrier != nil {
		barrier := r.claimBarrier
		r.claimBarrier = nil
		if err := barrier.wait(ctx); err != nil {
			return domain.QueueCandidate{}, err
		}
	}
	candidates := make([]domain.QueueCandidate, 0)
	for _, task := range store.tasks {
		if task.QueuedAt == nil {
			continue
		}
		execution, ok := store.executions[executionKey(task.TaskID, task.CurrentExecutionVersion)]
		if !ok {
			continue
		}
		candidates = append(candidates, domain.QueueCandidate{TaskID: task.TaskID, RunID: task.CurrentRunID,
			ExecutionVersion: task.CurrentExecutionVersion, TaskStatus: task.Status, ExecutionStatus: execution.Status,
			QueuedAt: *task.QueuedAt, CreatedAt: task.CreatedAt})
	}
	if len(candidates) == 0 {
		return domain.QueueCandidate{}, domain.ErrRepositoryNotFound
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].QueuedAt.Equal(candidates[j].QueuedAt) {
			return candidates[i].QueuedAt.Before(candidates[j].QueuedAt)
		}
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].TaskID < candidates[j].TaskID
	})
	return candidates[0], nil
}

func (r *phase1Repositories) Update(_ context.Context, token contracts.RuntimeWriteTx, update domain.TaskUpdate) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	task, ok := store.tasks[update.TaskID]
	if !ok || task.Status != update.ExpectedStatus || task.CurrentExecutionVersion != update.ExpectedCurrentExecutionVersion {
		return false, nil
	}
	task.Status, task.CurrentExecutionVersion = update.Status, update.CurrentExecutionVersion
	task.ResultSummary, task.ErrorCode, task.QueuedAt = update.ResultSummary, update.ErrorCode, update.QueuedAt
	task.StartedAt, task.EndedAt = update.StartedAt, update.EndedAt
	store.tasks[task.TaskID] = task
	return true, nil
}

func executionKey(taskID contracts.TaskID, version contracts.ExecutionVersion) string {
	return fmt.Sprintf("%s/%d", taskID, version)
}

type taskRepository struct{ *phase1Repositories }
type runRepository struct{ repositories *phase1Repositories }
type executionRepository struct{ repositories *phase1Repositories }
type receiptRepository struct{ repositories *phase1Repositories }
type taskLogRepository struct{ repositories *phase1Repositories }

func (r runRepository) Insert(_ context.Context, token contracts.RuntimeWriteTx, run domain.Run) error {
	store, err := strictStore(token)
	if err != nil {
		return err
	}
	if _, exists := store.runs[run.TaskID]; exists {
		return errors.New("duplicate Run")
	}
	store.runs[run.TaskID] = run
	return nil
}

func (r runRepository) FindByTask(_ context.Context, taskID contracts.TaskID) (domain.Run, error) {
	run, ok := r.repositories.executor.snapshot().runs[taskID]
	if !ok {
		return domain.Run{}, domain.ErrRepositoryNotFound
	}
	return run, nil
}

func (r runRepository) LockByTask(_ context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID) (domain.Run, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.Run{}, err
	}
	run, ok := store.runs[taskID]
	if !ok {
		return domain.Run{}, domain.ErrRepositoryNotFound
	}
	return run, nil
}

func (r runRepository) Update(_ context.Context, token contracts.RuntimeWriteTx, update domain.RunUpdate) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	run, runOK := store.runs[update.TaskID]
	task, taskOK := store.tasks[update.TaskID]
	if !runOK || !taskOK || run.RunID != update.RunID || run.Status != update.ExpectedStatus ||
		task.CurrentExecutionVersion != update.ExecutionVersion || task.CurrentRunID != update.RunID {
		return false, nil
	}
	run.Status, run.PlanID, run.CurrentStepID = update.Status, update.PlanID, update.CurrentStepID
	run.Context = append(json.RawMessage(nil), update.Context...)
	run.ErrorCode, run.StartedAt, run.EndedAt = update.ErrorCode, update.StartedAt, update.EndedAt
	store.runs[update.TaskID] = run
	return true, nil
}

func (r executionRepository) Insert(_ context.Context, token contracts.RuntimeWriteTx, execution domain.TaskExecution) error {
	store, err := strictStore(token)
	if err != nil {
		return err
	}
	key := executionKey(execution.TaskID, execution.ExecutionVersion)
	if _, exists := store.executions[key]; exists {
		return errors.New("duplicate Execution")
	}
	store.executions[key] = execution
	return nil
}

func (r executionRepository) FindByTaskVersion(_ context.Context, taskID contracts.TaskID, version contracts.ExecutionVersion) (domain.TaskExecution, error) {
	execution, ok := r.repositories.executor.snapshot().executions[executionKey(taskID, version)]
	if !ok {
		return domain.TaskExecution{}, domain.ErrRepositoryNotFound
	}
	return execution, nil
}

func (r executionRepository) LockByTaskVersion(_ context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID, version contracts.ExecutionVersion) (domain.TaskExecution, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.TaskExecution{}, err
	}
	execution, ok := store.executions[executionKey(taskID, version)]
	if !ok {
		return domain.TaskExecution{}, domain.ErrRepositoryNotFound
	}
	return execution, nil
}

func (r executionRepository) Update(_ context.Context, token contracts.RuntimeWriteTx, update domain.TaskExecutionUpdate) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	task, taskOK := store.tasks[update.TaskID]
	key := executionKey(update.TaskID, update.ExecutionVersion)
	execution, executionOK := store.executions[key]
	if !taskOK || !executionOK || task.CurrentExecutionVersion != update.ExecutionVersion ||
		execution.Status != update.ExpectedStatus || !sameWorker(execution.WorkerID, update.ExpectedWorkerID) {
		return false, nil
	}
	if update.ObservedConfigHash != nil && execution.ObservedConfigHash != nil {
		return false, nil
	}
	execution.Status, execution.WorkerID = update.Status, update.WorkerID
	if update.ObservedConfigHash != nil {
		execution.ObservedConfigHash = update.ObservedConfigHash
	}
	execution.ErrorCode, execution.InvariantCode = update.ErrorCode, update.InvariantCode
	execution.TerminationReason, execution.StartedAt, execution.EndedAt = update.TerminationReason, update.StartedAt, update.EndedAt
	store.executions[key] = execution
	return true, nil
}

func (r receiptRepository) Insert(_ context.Context, token contracts.RuntimeWriteTx, receipt domain.CommandReceipt) error {
	store, err := strictStore(token)
	if err != nil {
		return err
	}
	if _, exists := store.receipts[receipt.CommandID]; exists {
		return errors.New("duplicate Receipt")
	}
	store.receipts[receipt.CommandID] = receipt
	return nil
}

func (r receiptRepository) Find(_ context.Context, commandID domain.CommandID) (domain.CommandReceipt, error) {
	receipt, ok := r.repositories.executor.snapshot().receipts[commandID]
	if !ok {
		return domain.CommandReceipt{}, domain.ErrRepositoryNotFound
	}
	return receipt, nil
}

func (r receiptRepository) Lock(_ context.Context, token contracts.RuntimeWriteTx, commandID domain.CommandID) (domain.CommandReceipt, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.CommandReceipt{}, err
	}
	receipt, ok := store.receipts[commandID]
	if !ok {
		return domain.CommandReceipt{}, domain.ErrRepositoryNotFound
	}
	return receipt, nil
}

func (r taskLogRepository) Append(_ context.Context, token contracts.RuntimeWriteTx, log domain.TaskLog) error {
	store, err := strictStore(token)
	if err != nil {
		return err
	}
	store.logs = append(store.logs, log)
	return nil
}

func sameWorker(left, right *contracts.WorkerID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

type phase1ConfigSource struct{ config domain.AgentRuntimeConfig }

func (s phase1ConfigSource) LookupAgent(agentID contracts.AgentID) (domain.AgentRuntimeConfig, bool) {
	return s.config, s.config.ExecutionConfig.Agent.AgentID == agentID
}

type phase1Checkpoints struct {
	startupErr error
	seenMu     sync.Mutex
	seen       []contracts.RuntimeWriteTx
}

func (p *phase1Checkpoints) record(token contracts.RuntimeWriteTx) error {
	if _, err := strictStore(token); err != nil {
		return err
	}
	p.seenMu.Lock()
	p.seen = append(p.seen, token)
	p.seenMu.Unlock()
	return nil
}

func (p *phase1Checkpoints) SaveInitializationCheckpoint(_ context.Context, token contracts.RuntimeWriteTx, request domain.SaveRuntimeCheckpointRequest) error {
	return p.save(token, request)
}

func (p *phase1Checkpoints) SaveGeneratePlanExecutionCheckpoint(_ context.Context, token contracts.RuntimeWriteTx, request domain.SaveRuntimeCheckpointRequest) error {
	return p.save(token, request)
}

func (p *phase1Checkpoints) save(token contracts.RuntimeWriteTx, request domain.SaveRuntimeCheckpointRequest) error {
	store, err := strictStore(token)
	if err != nil {
		return err
	}
	if err := p.record(token); err != nil {
		return err
	}
	sequence := int64(1)
	for _, checkpoint := range store.checkpoints {
		if checkpoint.RunID == request.RunID && checkpoint.CheckpointSequence >= sequence {
			sequence = checkpoint.CheckpointSequence + 1
		}
	}
	store.checkpoints = append(store.checkpoints, domain.RuntimeCheckpoint{
		CheckpointID: contracts.CheckpointID(fmt.Sprintf("checkpoint-%s-%d", request.TaskID, sequence)),
		TaskID:       request.TaskID, RunID: request.RunID, ExecutionVersion: request.ExecutionVersion,
		ExecutionConfigHash: request.ExecutionConfigHash, NextAction: contracts.CheckpointNextActionGeneratePlan,
		CheckpointSequence: sequence,
	})
	return nil
}

func (p *phase1Checkpoints) latest(store *phase1Store, taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion) (domain.RuntimeCheckpoint, bool) {
	var latest domain.RuntimeCheckpoint
	found := false
	for _, checkpoint := range store.checkpoints {
		if checkpoint.TaskID == taskID && checkpoint.RunID == runID && checkpoint.ExecutionVersion == version &&
			(!found || checkpoint.CheckpointSequence > latest.CheckpointSequence) {
			latest, found = checkpoint, true
		}
	}
	return latest, found
}

func (p *phase1Checkpoints) LoadLatestForClaim(_ context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion, _ domain.ClaimCheckpointSource) (domain.ClaimCheckpointResult, error) {
	store, err := strictStore(token)
	if err != nil {
		return nil, err
	}
	if err := p.record(token); err != nil {
		return nil, err
	}
	checkpoint, ok := p.latest(store, taskID, runID, version)
	if !ok {
		return domain.ClaimCheckpointInvalid{ReasonCode: contracts.ReasonCodeCheckpointNotFound}, nil
	}
	return domain.ClaimCheckpointValid{Checkpoint: checkpoint}, nil
}

func (p *phase1Checkpoints) LoadLatestForExecutionDispatch(ctx context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion) (domain.ExecutionCheckpointResult, error) {
	result, err := p.LoadLatestForClaim(ctx, token, taskID, runID, version, domain.ClaimCheckpointSourceContinuation)
	if err != nil {
		return nil, err
	}
	switch value := result.(type) {
	case domain.ClaimCheckpointValid:
		return domain.ExecutionCheckpointValid{Checkpoint: value.Checkpoint}, nil
	case domain.ClaimCheckpointInvalid:
		return domain.ExecutionCheckpointInvalid{ReasonCode: value.ReasonCode}, nil
	default:
		return nil, errors.New("strict fake received unknown Checkpoint result")
	}
}

func (p *phase1Checkpoints) LoadLatestForStartupCleanup(_ context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion) (domain.StartupCleanupCheckpointResult, error) {
	store, err := strictStore(token)
	if err != nil {
		return nil, err
	}
	if err := p.record(token); err != nil {
		return nil, err
	}
	if p.startupErr != nil {
		return nil, p.startupErr
	}
	checkpoint, ok := p.latest(store, taskID, runID, version)
	if !ok {
		return domain.StartupCleanupCheckpointInvalid{ReasonCode: contracts.ReasonCodeCheckpointNotFound}, nil
	}
	return domain.StartupCleanupCheckpointValid{Checkpoint: checkpoint}, nil
}

type phase1ReportWriter struct{ seen atomic.Int64 }

func (w *phase1ReportWriter) EnsurePending(_ context.Context, token contracts.RuntimeWriteTx, request contracts.EnsurePendingReportRequest) (contracts.EnsurePendingReportResult, error) {
	store, err := strictStore(token)
	if err != nil {
		return nil, err
	}
	w.seen.Add(1)
	for _, existing := range store.reports {
		if existing.TaskID == request.TaskID {
			return contracts.EnsurePendingReportExisting{}, nil
		}
	}
	store.reports = append(store.reports, request)
	return contracts.EnsurePendingReportCreated{}, nil
}

type phase1DispatchRepository struct {
	clock       *phase1Clock
	checkpoints *phase1Checkpoints
}

func (r *phase1DispatchRepository) LockExecutionDispatch(_ context.Context, token contracts.RuntimeWriteTx, claim contracts.ExecutionClaim) (domain.ExecutionDispatchFacts, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.ExecutionDispatchFacts{}, err
	}
	task, taskOK := store.tasks[claim.TaskID]
	run, runOK := store.runs[claim.TaskID]
	execution, executionOK := store.executions[executionKey(claim.TaskID, claim.ExecutionVersion)]
	if !taskOK || !runOK || !executionOK {
		return domain.ExecutionDispatchFacts{}, domain.ErrRepositoryNotFound
	}
	facts := domain.ExecutionDispatchFacts{Task: task, Run: run, Execution: execution}
	if plan, ok := store.plans[claim.TaskID]; ok {
		planCopy := plan
		facts.Plan = &planCopy
	}
	if run.CurrentStepID != nil {
		if step, ok := store.steps[claim.TaskID][*run.CurrentStepID]; ok {
			stepCopy := step
			facts.Step = &stepCopy
		}
	}
	return facts, nil
}

func (r *phase1DispatchRepository) LockStep(_ context.Context, token contracts.RuntimeWriteTx, claim contracts.ExecutionClaim, stepID contracts.StepID) (domain.ExecutionStep, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.ExecutionStep{}, err
	}
	step, ok := store.steps[claim.TaskID][stepID]
	if !ok {
		return domain.ExecutionStep{}, domain.ErrRepositoryNotFound
	}
	return step, nil
}

func (r *phase1DispatchRepository) guardMatches(store *phase1Store, guard domain.ExecutionActionGuard) bool {
	task, taskOK := store.tasks[guard.Claim.TaskID]
	execution, executionOK := store.executions[executionKey(guard.Claim.TaskID, guard.Claim.ExecutionVersion)]
	checkpoint, checkpointOK := r.checkpoints.latest(store, guard.Claim.TaskID, guard.Claim.RunID, guard.Claim.ExecutionVersion)
	return taskOK && executionOK && checkpointOK && task.CurrentExecutionVersion == guard.Claim.ExecutionVersion &&
		execution.WorkerID != nil && *execution.WorkerID == guard.Claim.WorkerID &&
		checkpoint.CheckpointID == guard.CheckpointID && checkpoint.NextAction == guard.NextAction
}

func (r *phase1DispatchRepository) StartExecutionAction(_ context.Context, token contracts.RuntimeWriteTx, guard domain.ExecutionActionGuard) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	if !r.guardMatches(store, guard) {
		return false, nil
	}
	if guard.StepID != "" {
		step := store.steps[guard.Claim.TaskID][guard.StepID]
		if step.Status == contracts.StepStatusPending {
			step.Status = contracts.StepStatusRunning
			store.steps[guard.Claim.TaskID][guard.StepID] = step
		}
	}
	return true, nil
}

func (r *phase1DispatchRepository) ApplyPlannerCompleted(_ context.Context, token contracts.RuntimeWriteTx, request domain.ApplyPlannerCompletedRequest) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	if !r.guardMatches(store, request.Guard) {
		return false, nil
	}
	taskID := request.Guard.Claim.TaskID
	store.plans[taskID] = domain.ExecutionPlan{PlanID: request.Draft.PlanID}
	store.steps[taskID] = make(map[contracts.StepID]domain.ExecutionStep, len(request.Draft.Steps))
	for _, draft := range request.Draft.Steps {
		store.steps[taskID][draft.StepID] = domain.ExecutionStep{StepID: draft.StepID, Sequence: draft.Sequence,
			Type: draft.Type, Status: contracts.StepStatusPending, Input: append(json.RawMessage(nil), draft.Input...), ToolName: draft.ToolName}
	}
	run := store.runs[taskID]
	run.PlanID = &request.Draft.PlanID
	firstStepID := request.Draft.Steps[0].StepID
	run.CurrentStepID = &firstStepID
	store.runs[taskID] = run
	return true, r.appendCheckpoint(store, request.Guard.Claim, request.NextAction)
}

func (r *phase1DispatchRepository) ApplyStepCompleted(_ context.Context, token contracts.RuntimeWriteTx, request domain.ApplyStepCompletedRequest) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	if !r.guardMatches(store, request.Guard) {
		return false, nil
	}
	taskID := request.Guard.Claim.TaskID
	step := store.steps[taskID][request.Guard.StepID]
	step.Status = contracts.StepStatusCompleted
	store.steps[taskID][step.StepID] = step
	run := store.runs[taskID]
	run.Context = append(json.RawMessage(nil), request.Outcome.RunContext...)
	if request.Outcome.Continuation == contracts.StepContinuationNextStep {
		run.CurrentStepID = &request.Outcome.NextStepID
	}
	store.runs[taskID] = run
	return true, r.appendCheckpoint(store, request.Guard.Claim, request.NextAction)
}

func (r *phase1DispatchRepository) appendCheckpoint(store *phase1Store, claim contracts.ExecutionClaim, action contracts.CheckpointNextAction) error {
	latest, ok := r.checkpoints.latest(store, claim.TaskID, claim.RunID, claim.ExecutionVersion)
	if !ok {
		return domain.ErrRepositoryNotFound
	}
	store.checkpoints = append(store.checkpoints, domain.RuntimeCheckpoint{
		CheckpointID: contracts.CheckpointID(fmt.Sprintf("checkpoint-%s-%d", claim.TaskID, latest.CheckpointSequence+1)),
		TaskID:       claim.TaskID, RunID: claim.RunID, ExecutionVersion: claim.ExecutionVersion,
		ExecutionConfigHash: latest.ExecutionConfigHash, NextAction: action,
		CheckpointSequence: latest.CheckpointSequence + 1,
	})
	return nil
}

func (r *phase1DispatchRepository) TerminalizeExecution(_ context.Context, token contracts.RuntimeWriteTx, guard domain.ExecutionActionGuard, errorCode contracts.ErrorCode) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	if !r.guardMatches(store, guard) {
		return false, nil
	}
	now := r.clock.peek()
	task := store.tasks[guard.Claim.TaskID]
	run := store.runs[guard.Claim.TaskID]
	execution := store.executions[executionKey(guard.Claim.TaskID, guard.Claim.ExecutionVersion)]
	task.Status, task.ErrorCode, task.QueuedAt, task.EndedAt = contracts.TaskStatusFailed, &errorCode, nil, &now
	run.Status, run.ErrorCode, run.EndedAt = contracts.RunStatusFailed, &errorCode, &now
	execution.Status, execution.ErrorCode, execution.EndedAt = contracts.TaskExecutionStatusFailed, &errorCode, &now
	store.tasks[task.TaskID], store.runs[task.TaskID] = task, run
	store.executions[executionKey(task.TaskID, execution.ExecutionVersion)] = execution
	return true, nil
}

func (r *phase1DispatchRepository) TerminalizeCheckpointInvalid(ctx context.Context, token contracts.RuntimeWriteTx, request domain.TerminalizeCheckpointInvalidRequest) (bool, error) {
	return r.TerminalizeExecution(ctx, token, domain.ExecutionActionGuard{Claim: request.Claim}, contracts.ErrorCodeCheckpointInvalid)
}

func (r *phase1DispatchRepository) FinalizeExecution(_ context.Context, token contracts.RuntimeWriteTx, guard domain.ExecutionActionGuard) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	if !r.guardMatches(store, guard) {
		return false, nil
	}
	now := r.clock.peek()
	task := store.tasks[guard.Claim.TaskID]
	run := store.runs[guard.Claim.TaskID]
	execution := store.executions[executionKey(guard.Claim.TaskID, guard.Claim.ExecutionVersion)]
	task.Status, task.QueuedAt, task.EndedAt = contracts.TaskStatusCompleted, nil, &now
	run.Status, run.EndedAt = contracts.RunStatusCompleted, &now
	execution.Status, execution.EndedAt = contracts.TaskExecutionStatusCompleted, &now
	store.tasks[task.TaskID], store.runs[task.TaskID] = task, run
	store.executions[executionKey(task.TaskID, execution.ExecutionVersion)] = execution
	return true, nil
}

func (r *phase1DispatchRepository) ConfirmExecutionWaitingApproval(context.Context, contracts.RuntimeWriteTx, contracts.ExecutionClaim, contracts.StepID) (bool, error) {
	return false, nil
}

func (r *phase1DispatchRepository) ConfirmExecutionTerminal(_ context.Context, token contracts.RuntimeWriteTx, claim contracts.ExecutionClaim) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	task := store.tasks[claim.TaskID]
	run := store.runs[claim.TaskID]
	execution := store.executions[executionKey(claim.TaskID, claim.ExecutionVersion)]
	return task.Status.Terminal() && run.Status.Terminal() && execution.Status.Ended(), nil
}

type phase1TerminationRepository struct{ repositories *phase1Repositories }

func (r *phase1TerminationRepository) LockTerminationFacts(ctx context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID) (domain.TerminationFacts, error) {
	store, err := strictStore(token)
	if err != nil {
		return domain.TerminationFacts{}, err
	}
	if r.repositories.terminationBarrier != nil {
		barrier := r.repositories.terminationBarrier
		r.repositories.terminationBarrier = nil
		if err := barrier.wait(ctx); err != nil {
			return domain.TerminationFacts{}, err
		}
	}
	task, taskOK := store.tasks[taskID]
	run, runOK := store.runs[taskID]
	execution, executionOK := store.executions[executionKey(taskID, task.CurrentExecutionVersion)]
	if !taskOK || !runOK || !executionOK {
		return domain.TerminationFacts{}, domain.ErrRepositoryNotFound
	}
	facts := domain.TerminationFacts{Task: task, Run: run, Execution: execution}
	if run.CurrentStepID != nil {
		if step, ok := store.steps[taskID][*run.CurrentStepID]; ok {
			facts.Step = &domain.TerminationStep{StepID: step.StepID, Status: step.Status}
		}
	}
	return facts, nil
}

func (r *phase1TerminationRepository) ApplyTermination(_ context.Context, token contracts.RuntimeWriteTx, request domain.ApplyTerminationRequest) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	task, taskOK := store.tasks[request.TaskID]
	run, runOK := store.runs[request.TaskID]
	key := executionKey(request.TaskID, request.ExpectedExecutionVersion)
	execution, executionOK := store.executions[key]
	if !taskOK || !runOK || !executionOK || task.CurrentExecutionVersion != request.ExpectedExecutionVersion ||
		task.Status != request.ExpectedTaskStatus || run.Status != request.ExpectedRunStatus || execution.Status != request.ExpectedExecutionStatus {
		return false, nil
	}
	if request.ExpectedStepStatus != nil {
		if run.CurrentStepID == nil {
			return false, nil
		}
		step := store.steps[request.TaskID][*run.CurrentStepID]
		if step.Status != *request.ExpectedStepStatus {
			return false, nil
		}
		step.Status = contracts.StepStatusFailed
		store.steps[request.TaskID][step.StepID] = step
	}
	task.Status, task.ErrorCode, task.QueuedAt, task.EndedAt = request.TaskStatus, &request.TaskErrorCode, nil, &request.EndedAt
	run.Status, run.ErrorCode, run.EndedAt = contracts.RunStatusFailed, &request.RunErrorCode, &request.EndedAt
	execution.Status, execution.ErrorCode = contracts.TaskExecutionStatusFailed, request.ExecutionErrorCode
	execution.TerminationReason = &request.TerminationReason
	if !request.PreserveExecutionEndedAt {
		execution.EndedAt = &request.EndedAt
	}
	store.tasks[request.TaskID], store.runs[request.TaskID], store.executions[key] = task, run, execution
	return true, nil
}

type phase1StartupRepository struct{ repositories *phase1Repositories }

func (r *phase1StartupRepository) LockLegacyRunningExecutions(_ context.Context, token contracts.RuntimeWriteTx, currentWorkerID contracts.WorkerID) ([]domain.StartupCleanupFacts, error) {
	store, err := strictStore(token)
	if err != nil {
		return nil, err
	}
	result := make([]domain.StartupCleanupFacts, 0)
	for _, task := range store.tasks {
		execution, ok := store.executions[executionKey(task.TaskID, task.CurrentExecutionVersion)]
		if !ok || execution.Status != contracts.TaskExecutionStatusRunning || execution.WorkerID == nil || *execution.WorkerID == currentWorkerID {
			continue
		}
		run := store.runs[task.TaskID]
		facts := domain.StartupCleanupFacts{Task: task, Run: run, Execution: execution}
		if run.CurrentStepID != nil {
			if step, ok := store.steps[task.TaskID][*run.CurrentStepID]; ok {
				facts.Step = &domain.StartupCleanupStep{StepID: step.StepID, Type: step.Type, Status: step.Status, ToolName: step.ToolName}
			}
		}
		result = append(result, facts)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Task.TaskID < result[j].Task.TaskID })
	return result, nil
}

func (r *phase1StartupRepository) ApplyStartupCleanup(_ context.Context, token contracts.RuntimeWriteTx, request domain.ApplyStartupCleanupRequest) (bool, error) {
	store, err := strictStore(token)
	if err != nil {
		return false, err
	}
	task, taskOK := store.tasks[request.TaskID]
	run, runOK := store.runs[request.TaskID]
	key := executionKey(request.TaskID, request.ExecutionVersion)
	execution, executionOK := store.executions[key]
	if !taskOK || !runOK || !executionOK || task.QueuedAt != nil || task.CurrentExecutionVersion != request.ExecutionVersion ||
		task.Status != request.ExpectedTaskStatus || run.Status != request.ExpectedRunStatus ||
		execution.Status != request.ExpectedExecutionStatus || execution.WorkerID == nil || *execution.WorkerID != request.ExpectedWorkerID {
		return false, nil
	}
	execution.ErrorCode = &request.ExecutionErrorCode
	execution.EndedAt = &request.EndedAt
	switch request.Disposition {
	case domain.StartupCleanupInterrupt:
		execution.Status = contracts.TaskExecutionStatusInterrupted
	case domain.StartupCleanupTerminal:
		execution.Status = contracts.TaskExecutionStatusFailed
		execution.TerminationReason = request.TerminationReason
		task.Status, task.ErrorCode, task.EndedAt = contracts.TaskStatusFailed, request.TaskErrorCode, &request.EndedAt
		run.Status, run.ErrorCode, run.EndedAt = contracts.RunStatusFailed, request.TaskErrorCode, &request.EndedAt
	default:
		return false, errors.New("unknown StartupCleanup disposition")
	}
	store.tasks[request.TaskID], store.runs[request.TaskID], store.executions[key] = task, run, execution
	return true, nil
}

type successfulPlanner struct{}

func (successfulPlanner) GeneratePlan(_ context.Context, request domain.PlannerRequest) (domain.PlannerOutcome, error) {
	return domain.PlannerOutcomeCompleted{Draft: domain.ValidatedPlanDraft{
		PlanID: contracts.PlanID("plan-" + request.TaskID), Goal: "complete task",
		Steps: []domain.PlanStepDraft{{StepID: contracts.StepID("step-" + request.TaskID), Sequence: 1, Type: contracts.StepTypeModelCall}},
	}}, nil
}

type blockingPlanner struct{ entered chan struct{} }

func (p *blockingPlanner) GeneratePlan(ctx context.Context, _ domain.PlannerRequest) (domain.PlannerOutcome, error) {
	select {
	case <-p.entered:
	default:
		close(p.entered)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type successfulStepExecutor struct{}

func (successfulStepExecutor) ExecuteStep(_ context.Context, _ domain.StepExecutionRequest) (domain.StepOutcome, error) {
	return domain.StepOutcomeCompleted{Output: json.RawMessage(`{"ok":true}`), RunContext: json.RawMessage(`{}`),
		Continuation: contracts.StepContinuationFinalizeRun}, nil
}

type phase1Harness struct {
	workerID     contracts.WorkerID
	executor     *phase1Executor
	repositories *phase1Repositories
	clock        *phase1Clock
	configs      phase1ConfigSource
	checkpoints  *phase1Checkpoints
	reports      *phase1ReportWriter
	dispatch     *phase1DispatchRepository
	activeCalls  *activecall.Registry
	create       *domain.CreateTaskService
	claim        *domain.ClaimTaskService
	execute      *domain.ExecuteTaskService
	cancel       *domain.CancelTaskService
	expire       *domain.ExpireTaskService
	startup      *domain.StartupCleanupService
}

func newPhase1Harness(t *testing.T) *phase1Harness {
	t.Helper()
	loaded, err := business.Load("../../../configs/business.json")
	if err != nil {
		t.Fatalf("load Phase 1 business config: %v", err)
	}
	agent, ok := loaded.Lookup("agent-default")
	if !ok {
		t.Fatal("agent-default missing from Phase 1 business config")
	}
	allowedTools := make([]string, len(agent.ExecutionConfig.Agent.AllowedTools))
	for index, tool := range agent.ExecutionConfig.Agent.AllowedTools {
		allowedTools[index] = string(tool)
	}
	config := domain.AgentRuntimeConfig{
		TaskTimeout: agent.TaskTimeout, ExecutionConfig: agent.ExecutionConfig,
		PlanningToolCatalogSelector: contracts.PlanningToolCatalogSelector{
			CatalogID: agent.CatalogID, AllowedTools: allowedTools, ExpectedRegistryVersion: "registry-v1",
			ExpectedSnapshotHash: contracts.CatalogSnapshotHash(strings.Repeat("a", 64)),
		},
	}
	executor := newPhase1Executor()
	repositories := &phase1Repositories{executor: executor}
	clock := &phase1Clock{now: time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC), tick: time.Second}
	configs := phase1ConfigSource{config: config}
	checkpoints := &phase1Checkpoints{}
	reports := &phase1ReportWriter{}
	dispatch := &phase1DispatchRepository{clock: clock, checkpoints: checkpoints}
	registry := activecall.NewRegistry()
	workerID := contracts.WorkerID("phase1-worker")
	tasks := taskRepository{repositories}
	runs := runRepository{repositories}
	executions := executionRepository{repositories}
	receipts := receiptRepository{repositories}
	logs := taskLogRepository{repositories}
	terminations := &phase1TerminationRepository{repositories: repositories}
	startupRepository := &phase1StartupRepository{repositories: repositories}
	policy := lifecycle.New()

	create, err := domain.NewCreateTaskService(domain.CreateTaskDependencies{Executor: executor, Tasks: tasks,
		Runs: runs, Executions: executions, Receipts: receipts, TaskLogs: logs, Clock: clock,
		Configs: configs, Checkpoints: checkpoints, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := domain.NewClaimTaskService(domain.ClaimTaskDependencies{Executor: executor, Tasks: tasks,
		Runs: runs, Executions: executions, TaskLogs: logs, Clock: clock, Configs: configs,
		Checkpoints: checkpoints, Reports: reports, Policy: policy, RuntimeWorker: workerID})
	if err != nil {
		t.Fatal(err)
	}
	execute, err := domain.NewExecuteTaskService(domain.ExecuteTaskDependencies{Executor: executor, Dispatch: dispatch,
		Checkpoints: checkpoints, Clock: clock, Configs: configs, Planner: successfulPlanner{},
		Steps: successfulStepExecutor{}, ActiveCalls: registry, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	cancel, err := domain.NewCancelTaskService(domain.CancelTaskDependencies{Executor: executor,
		Terminations: terminations, Receipts: receipts, Reports: reports, Clock: clock, Configs: configs,
		TaskLogs: logs, ActiveCalls: registry, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	expire, err := domain.NewExpireTaskService(domain.ExpireTaskDependencies{Executor: executor,
		Terminations: terminations, Reports: reports, Clock: clock, Configs: configs,
		TaskLogs: logs, ActiveCalls: registry, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	startup, err := domain.NewStartupCleanupService(domain.StartupCleanupDependencies{Executor: executor,
		Repository: startupRepository, Checkpoints: checkpoints, Reports: reports, Clock: clock,
		Configs: configs, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return &phase1Harness{workerID: workerID, executor: executor, repositories: repositories, clock: clock,
		configs: configs, checkpoints: checkpoints, reports: reports, dispatch: dispatch, activeCalls: registry,
		create: create, claim: claim, execute: execute, cancel: cancel, expire: expire, startup: startup}
}

func (h *phase1Harness) replacePlanner(t *testing.T, planner domain.PlannerPort) {
	t.Helper()
	service, err := domain.NewExecuteTaskService(domain.ExecuteTaskDependencies{Executor: h.executor, Dispatch: h.dispatch,
		Checkpoints: h.checkpoints, Clock: h.clock, Configs: h.configs, Planner: planner,
		Steps: successfulStepExecutor{}, ActiveCalls: h.activeCalls, Policy: lifecycle.New()})
	if err != nil {
		t.Fatal(err)
	}
	h.execute = service
}

func (h *phase1Harness) createTask(t *testing.T, commandID domain.CommandID, input string) domain.TaskCreated {
	t.Helper()
	created, err := h.create.CreateTask(context.Background(), domain.CreateTaskRequest{
		CommandID: commandID, AgentID: "agent-default", TaskInput: input, OperatorID: "operator",
	})
	if err != nil {
		t.Fatalf("CreateTask(%s): %v", commandID, err)
	}
	return created
}

type claimCallResult struct {
	result contracts.ClaimResult
	err    error
}

type cancelCallResult struct {
	result domain.TaskCancelled
	err    error
}

type executeCallResult struct {
	result contracts.ExecuteResult
	err    error
}

type expireCallResult struct {
	result domain.ExpireTaskResult
	err    error
}

func awaitClosed(t *testing.T, channel <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

func awaitClaimCall(t *testing.T, result <-chan claimCallResult) claimCallResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-time.After(5 * time.Second):
		t.Fatal("Claim call did not complete")
		return claimCallResult{}
	}
}

func awaitCancelCall(t *testing.T, result <-chan cancelCallResult) cancelCallResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel call did not complete")
		return cancelCallResult{}
	}
}

func awaitExecuteCall(t *testing.T, result <-chan executeCallResult) executeCallResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-time.After(5 * time.Second):
		t.Fatal("Execute call did not complete")
		return executeCallResult{}
	}
}

func awaitExpireCall(t *testing.T, result <-chan expireCallResult) expireCallResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-time.After(5 * time.Second):
		t.Fatal("ExpireTask call did not complete")
		return expireCallResult{}
	}
}

func runStartupGate(ctx context.Context, cleanup *domain.StartupCleanupService, workerID contracts.WorkerID, start func()) (domain.StartupCleanupSummary, error) {
	summary, err := cleanup.StartupCleanup(ctx, workerID)
	if err != nil {
		return domain.StartupCleanupSummary{}, err
	}
	start()
	return summary, nil
}

func assertTerminalSuccess(t *testing.T, store *phase1Store, taskID contracts.TaskID) {
	t.Helper()
	task := store.tasks[taskID]
	run := store.runs[taskID]
	execution := store.executions[executionKey(taskID, task.CurrentExecutionVersion)]
	if task.Status != contracts.TaskStatusCompleted || run.Status != contracts.RunStatusCompleted ||
		execution.Status != contracts.TaskExecutionStatusCompleted || task.QueuedAt != nil {
		t.Fatalf("terminal success facts for %s = Task:%s Run:%s Execution:%s queued=%v",
			taskID, task.Status, run.Status, execution.Status, task.QueuedAt)
	}
}

func assertCancelled(t *testing.T, store *phase1Store, taskID contracts.TaskID) {
	t.Helper()
	task := store.tasks[taskID]
	run := store.runs[taskID]
	execution := store.executions[executionKey(taskID, task.CurrentExecutionVersion)]
	if task.Status != contracts.TaskStatusCancelled || run.Status != contracts.RunStatusFailed ||
		execution.Status != contracts.TaskExecutionStatusFailed || execution.TerminationReason == nil ||
		*execution.TerminationReason != contracts.TerminationReasonCancelled || len(store.reports) != 1 {
		t.Fatalf("Cancel facts = Task:%+v Run:%+v Execution:%+v Reports:%d", task, run, execution, len(store.reports))
	}
}

func assertTimedOut(t *testing.T, store *phase1Store, taskID contracts.TaskID) {
	t.Helper()
	task := store.tasks[taskID]
	run := store.runs[taskID]
	execution := store.executions[executionKey(taskID, task.CurrentExecutionVersion)]
	if task.Status != contracts.TaskStatusFailed || task.ErrorCode == nil || *task.ErrorCode != contracts.ErrorCodeTaskTimeout ||
		run.Status != contracts.RunStatusFailed || execution.Status != contracts.TaskExecutionStatusFailed ||
		execution.TerminationReason == nil || *execution.TerminationReason != contracts.TerminationReasonTimedOut || len(store.reports) != 1 {
		t.Fatalf("Timeout facts = Task:%+v Run:%+v Execution:%+v Reports:%d", task, run, execution, len(store.reports))
	}
}

func assertEvents(t *testing.T, logs []domain.TaskLog, want map[string]int) {
	t.Helper()
	seen := make(map[string]int)
	for _, log := range logs {
		seen[log.Event]++
	}
	for event, count := range want {
		if seen[event] != count {
			t.Fatalf("TaskLog event %s count = %d, want %d; all=%v", event, seen[event], count, seen)
		}
	}
}

var (
	_ contracts.RuntimeWriteExecutor     = (*phase1Executor)(nil)
	_ domain.TaskRepository              = taskRepository{}
	_ domain.RunRepository               = runRepository{}
	_ domain.TaskExecutionRepository     = executionRepository{}
	_ domain.CommandReceiptRepository    = receiptRepository{}
	_ domain.TaskLogRepository           = taskLogRepository{}
	_ domain.RuntimeCheckpointPort       = (*phase1Checkpoints)(nil)
	_ contracts.PendingReportWriter      = (*phase1ReportWriter)(nil)
	_ domain.ExecutionDispatchRepository = (*phase1DispatchRepository)(nil)
	_ domain.TerminationRepository       = (*phase1TerminationRepository)(nil)
	_ domain.StartupCleanupRepository    = (*phase1StartupRepository)(nil)
	_ domain.PlannerPort                 = successfulPlanner{}
	_ domain.StepExecutorPort            = successfulStepExecutor{}
)

// Package activecall 管理 Task Runtime 进程内正在准备或执行的外部调用。
//
// Registry 只关闭取消与动作启动之间的进程内竞态，不是持久化事实。
package activecall

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	// ErrDuplicate 表示同一个执行所有权键已经存在活动句柄。
	ErrDuplicate = errors.New("duplicate Active Call")
	// ErrNotPrepared 表示句柄已失效或不再处于 PREPARED。
	ErrNotPrepared = errors.New("Active Call is not PREPARED")
	// ErrInvalidCancellationCause 表示调用方使用了非冻结取消原因。
	ErrInvalidCancellationCause = errors.New("invalid Active Call cancellation cause")
)

// State 是 Active Call 的进程内阶段。
type State string

const (
	StatePrepared State = "PREPARED"
	StateActive   State = "ACTIVE"
)

// CancellationCause 是共享执行契约定义的封闭取消原因。
type CancellationCause = contracts.ExecutionCancellationCause

const (
	CauseTaskCancelled   = contracts.ExecutionCancellationCauseTaskCancelled
	CauseTaskTimedOut    = contracts.ExecutionCancellationCauseTaskTimedOut
	CauseActionTimeout   = contracts.ExecutionCancellationCauseActionTimeout
	CauseRuntimeShutdown = contracts.ExecutionCancellationCauseRuntimeShutdown
	CauseLockLost        = contracts.ExecutionCancellationCauseLockLost
)

// Key 唯一标识某个 Worker 当前拥有的执行版本。
type Key struct {
	TaskID           contracts.TaskID
	ExecutionVersion contracts.ExecutionVersion
	WorkerID         contracts.WorkerID
}

// Metadata 描述被控制的冻结动作。
type Metadata struct {
	ActionKind      contracts.CheckpointNextAction
	StepID          contracts.StepID
	ToolExecutionID contracts.ToolExecutionID
}

type entry struct {
	key      Key
	metadata Metadata
	state    State
	ctx      context.Context
	cancel   context.CancelCauseFunc
}

// Registry 保存当前进程的 Active Call。
type Registry struct {
	mu      sync.Mutex
	entries map[Key]*entry
}

// Handle 是一次成功预登记返回的能力句柄。
type Handle struct {
	registry *Registry
	entry    *entry
	once     sync.Once
}

// NewRegistry 创建空 Registry。
func NewRegistry() *Registry {
	return &Registry{entries: make(map[Key]*entry)}
}

// Prepare 在任何动作开始事务之前原子登记 PREPARED 句柄。
func (r *Registry) Prepare(parent context.Context, key Key, metadata Metadata) (*Handle, error) {
	if r == nil {
		return nil, errors.New("prepare Active Call: Registry is required")
	}
	if parent == nil {
		return nil, errors.New("prepare Active Call: context is required")
	}
	if err := validate(key, metadata); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	candidate := &entry{key: key, metadata: metadata, state: StatePrepared, ctx: ctx, cancel: cancel}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[key]; exists {
		cancel(ErrDuplicate)
		return nil, fmt.Errorf("prepare Active Call: %w", ErrDuplicate)
	}
	r.entries[key] = candidate
	return &Handle{registry: r, entry: candidate}, nil
}

// Context 返回必须直接用于外部调用的同一可取消 context。
func (h *Handle) Context() context.Context {
	if h == nil || h.entry == nil {
		return nil
	}
	return h.entry.ctx
}

// Activate 在动作开始事务提交后原子执行 PREPARED 到 ACTIVE。
func (h *Handle) Activate() error {
	if h == nil || h.registry == nil || h.entry == nil {
		return fmt.Errorf("activate Active Call: %w", ErrNotPrepared)
	}
	r := h.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.entries[h.entry.key]
	if !exists || current != h.entry || current.state != StatePrepared {
		return fmt.Errorf("activate Active Call: %w", ErrNotPrepared)
	}
	current.state = StateActive
	return nil
}

// Unregister 幂等注销当前句柄；旧句柄不会删除同 key 的新登记。
func (h *Handle) Unregister() {
	if h == nil || h.registry == nil || h.entry == nil {
		return
	}
	h.once.Do(func() {
		r := h.registry
		r.mu.Lock()
		if r.entries[h.entry.key] == h.entry {
			delete(r.entries, h.entry.key)
		}
		r.mu.Unlock()
		h.entry.cancel(context.Canceled)
	})
}

// Cancel 以冻结原因幂等取消匹配的 PREPARED 或 ACTIVE 句柄。
func (r *Registry) Cancel(key Key, cause CancellationCause) (bool, error) {
	if !cause.Valid() {
		return false, ErrInvalidCancellationCause
	}
	if r == nil {
		return false, nil
	}
	r.mu.Lock()
	current := r.entries[key]
	r.mu.Unlock()
	if current == nil {
		return false, nil
	}
	current.cancel(cause)
	return true, nil
}

// CancelAll 以冻结原因取消当前快照中的全部 PREPARED 和 ACTIVE 句柄。
func (r *Registry) CancelAll(cause CancellationCause) error {
	if !cause.Valid() {
		return ErrInvalidCancellationCause
	}
	if r == nil {
		return nil
	}
	r.mu.Lock()
	entries := make([]*entry, 0, len(r.entries))
	for _, current := range r.entries {
		entries = append(entries, current)
	}
	r.mu.Unlock()
	for _, current := range entries {
		current.cancel(cause)
	}
	return nil
}

// State 返回匹配句柄的当前阶段，仅用于生命周期协调和测试观察。
func (r *Registry) State(key Key) (State, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.entries[key]
	if !exists {
		return "", false
	}
	return current.state, true
}

func validate(key Key, metadata Metadata) error {
	if key.TaskID == "" || !key.ExecutionVersion.Valid() || key.WorkerID == "" {
		return errors.New("prepare Active Call: task ID, execution version and worker ID are required")
	}
	if !metadata.ActionKind.Valid() || metadata.ActionKind == contracts.CheckpointNextActionFinalizeRun {
		return errors.New("prepare Active Call: action kind must be an external action")
	}
	if metadata.ActionKind != contracts.CheckpointNextActionGeneratePlan && metadata.StepID == "" {
		return errors.New("prepare Active Call: step ID is required for a Step action")
	}
	return nil
}

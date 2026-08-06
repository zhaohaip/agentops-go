package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

func TestTimeoutScannerImmediatelySubmitsEachCandidate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	source := &fakeTimeoutCandidateSource{candidates: []TimeoutCandidate{
		{TaskID: "task-1", ObservedExecutionVersion: 1},
		{TaskID: "task-2", ObservedExecutionVersion: 2},
	}}
	expirer := &fakeTaskExpirer{afterCall: func(count int) {
		if count == 2 {
			cancel(context.Canceled)
		}
	}}
	scanner, err := NewTimeoutScanner(source, expirer, time.Second)
	if err != nil {
		t.Fatalf("NewTimeoutScanner() error = %v", err)
	}
	if err := scanner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	want := []taskruntime.ExpireTaskRequest{
		{TaskID: "task-1", ObservedExecutionVersion: 1},
		{TaskID: "task-2", ObservedExecutionVersion: 2},
	}
	if len(expirer.requests) != len(want) {
		t.Fatalf("ExpireTask calls = %d, want %d", len(expirer.requests), len(want))
	}
	for index := range want {
		if expirer.requests[index] != want[index] {
			t.Fatalf("request[%d] = %+v, want %+v", index, expirer.requests[index], want[index])
		}
	}
}

func TestTimeoutScannerRejectsIntervalsOverFiveSeconds(t *testing.T) {
	t.Parallel()
	source := &fakeTimeoutCandidateSource{}
	expirer := &fakeTaskExpirer{}
	for _, interval := range []time.Duration{0, -time.Second, 5*time.Second + time.Nanosecond} {
		if _, err := NewTimeoutScanner(source, expirer, interval); err == nil {
			t.Fatalf("NewTimeoutScanner(%s) succeeded", interval)
		}
	}
	if _, err := NewTimeoutScanner(source, expirer, 5*time.Second); err != nil {
		t.Fatalf("NewTimeoutScanner(5s) error = %v", err)
	}
}

func TestTimeoutScannerStopsOnSystemError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("database unavailable")
	scanner, err := NewTimeoutScanner(&fakeTimeoutCandidateSource{candidates: []TimeoutCandidate{
		{TaskID: "task-1", ObservedExecutionVersion: 1},
	}}, &fakeTaskExpirer{fail: wantErr}, time.Second)
	if err != nil {
		t.Fatalf("NewTimeoutScanner() error = %v", err)
	}
	if err := scanner.Run(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
}

func TestTimeoutScannerProcessesTwentyCandidatesWithinOneScan(t *testing.T) {
	t.Parallel()
	candidates := make([]TimeoutCandidate, 20)
	for index := range candidates {
		candidates[index] = TimeoutCandidate{
			TaskID: contracts.TaskID(fmt.Sprintf("task-%02d", index)), ObservedExecutionVersion: 1,
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	expirer := &fakeTaskExpirer{afterCall: func(count int) {
		if count == len(candidates) {
			cancel(context.Canceled)
		}
	}}
	scanner, err := NewTimeoutScanner(&fakeTimeoutCandidateSource{candidates: candidates}, expirer, 5*time.Second)
	if err != nil {
		t.Fatalf("NewTimeoutScanner() error = %v", err)
	}
	startedAt := time.Now()
	if err := scanner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if len(expirer.requests) != len(candidates) {
		t.Fatalf("ExpireTask calls = %d, want %d", len(expirer.requests), len(candidates))
	}
	if elapsed := time.Since(startedAt); elapsed >= 5*time.Second {
		t.Fatalf("single 20-candidate scan took %s", elapsed)
	}
}

type fakeTimeoutCandidateSource struct {
	candidates []TimeoutCandidate
	fail       error
}

func (s *fakeTimeoutCandidateSource) ListTimeoutCandidates(context.Context) ([]TimeoutCandidate, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	return append([]TimeoutCandidate(nil), s.candidates...), nil
}

type fakeTaskExpirer struct {
	mu        sync.Mutex
	requests  []taskruntime.ExpireTaskRequest
	fail      error
	afterCall func(int)
}

func (e *fakeTaskExpirer) ExpireTask(
	_ context.Context,
	request taskruntime.ExpireTaskRequest,
) (taskruntime.ExpireTaskResult, error) {
	e.mu.Lock()
	e.requests = append(e.requests, request)
	count := len(e.requests)
	e.mu.Unlock()
	if e.afterCall != nil {
		e.afterCall(count)
	}
	if e.fail != nil {
		return nil, e.fail
	}
	return taskruntime.ExpireTaskExpired{}, nil
}

var (
	_ TimeoutCandidateSource = (*fakeTimeoutCandidateSource)(nil)
	_ TaskExpirer            = (*fakeTaskExpirer)(nil)
)

package planner

import (
	"context"
	"errors"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// CatalogConsumer 是 Planner 对唯一共享 PlanningToolCatalogPort 的无状态消费者。
type CatalogConsumer struct {
	port contracts.PlanningToolCatalogPort
}

// NewCatalogConsumer 创建不读取 Registry、也不缓存跨调用快照的 Catalog consumer。
func NewCatalogConsumer(port contracts.PlanningToolCatalogPort) CatalogConsumer {
	return CatalogConsumer{port: port}
}

// Load 加载并完整验证一次 Planning Tool Snapshot。
func (c CatalogConsumer) Load(
	ctx context.Context,
	selector contracts.PlanningToolCatalogSelector,
) (contracts.PlanningToolSnapshot, error) {
	if ctx == nil {
		return contracts.PlanningToolSnapshot{}, newCatalogRuntimeFatal(nil)
	}
	if err := ctx.Err(); err != nil {
		return contracts.PlanningToolSnapshot{}, err
	}
	if err := contracts.ValidatePlanningToolCatalogSelector(selector); err != nil {
		return contracts.PlanningToolSnapshot{}, err
	}
	if c.port == nil {
		return contracts.PlanningToolSnapshot{}, newCatalogRuntimeFatal(nil)
	}

	snapshot, err := c.port.LoadPlanningToolSnapshot(ctx, selector)
	if contextErr := ctx.Err(); contextErr != nil {
		return contracts.PlanningToolSnapshot{}, contextErr
	}
	if err != nil {
		if !planningToolSnapshotIsZero(snapshot) {
			return contracts.PlanningToolSnapshot{}, newCatalogRuntimeFatal(err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return contracts.PlanningToolSnapshot{}, err
		}
		var typed *contracts.PlanningToolCatalogError
		if !errors.As(err, &typed) || typed == nil || !typed.Kind.Valid() || !typed.CauseCode.Valid() {
			return contracts.PlanningToolSnapshot{}, newCatalogRuntimeFatal(err)
		}
		return contracts.PlanningToolSnapshot{}, err
	}
	if validationErr := contracts.ValidatePlanningToolSnapshot(selector, snapshot); validationErr != nil {
		return contracts.PlanningToolSnapshot{}, validationErr
	}
	return snapshot, nil
}

func planningToolSnapshotIsZero(snapshot contracts.PlanningToolSnapshot) bool {
	return snapshot.SchemaVersion == 0 && snapshot.RegistryVersion == "" &&
		snapshot.SnapshotHash == "" && snapshot.Tools == nil
}

func newCatalogRuntimeFatal(cause error) error {
	return contracts.NewPlanningToolCatalogError(
		contracts.PlanningToolCatalogErrorRuntimeFatal,
		nil,
		contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent,
		cause,
	)
}

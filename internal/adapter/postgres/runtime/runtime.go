package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const advisoryLockKey int64 = 0x4167656e744f7073 // ASCII: AgentOps

var (
	// ErrAdvisoryLockUnavailable 表示当前 Database 已有 AgentOps Runtime 持锁。
	ErrAdvisoryLockUnavailable = errors.New("PostgreSQL advisory lock is unavailable")
	// ErrLockConnectionLost 表示持锁连接已经失效，当前进程不可再写入。
	ErrLockConnectionLost = errors.New("PostgreSQL advisory lock connection is lost")
	// ErrWriteUnavailable 表示 Runtime 已失锁或进入关闭状态。
	ErrWriteUnavailable = errors.New("PostgreSQL runtime write capability is unavailable")
	// ErrReadUnavailable 表示 Runtime 已进入关闭状态，不再接纳新的读取。
	ErrReadUnavailable = errors.New("PostgreSQL runtime read capability is unavailable")
	// ErrReadOnlyPoolWrite 表示调用方尝试通过普通连接池执行写入口。
	ErrReadOnlyPoolWrite = errors.New("PostgreSQL read pool does not execute writes")
	// ErrUnsafeDatabaseIdentity 表示数据库身份未满足读写权限隔离要求。
	ErrUnsafeDatabaseIdentity = errors.New("PostgreSQL database identities are not safely separated")
)

type lifecycleState uint8

const (
	stateOpen lifecycleState = iota
	stateMonitoring
	stateStartupFailed
	stateLost
	stateClosing
	stateClosed
)

// Runtime 拥有一个 Database 对应的 PostgreSQL 运行期资源。
//
// 持锁连接不会重建；普通连接池在协议层默认只读，并且不对外暴露底层连接池。
type Runtime struct {
	lockConn      *pgx.Conn
	migrationConn *pgx.Conn
	migrator      *migration.Runner
	reader        *ReadPool
	clock         *DatabaseClock
	executor      *writeExecutor

	lockCheckInterval time.Duration
	lockCheckTimeout  time.Duration
	gate              chan struct{}
	gateMu            sync.Mutex
	gateWaiters       int
	lockConnUse       sync.Mutex
	failure           chan error
	monitorDone       chan struct{}

	stateMu         sync.RWMutex
	state           lifecycleState
	acceptingWrites bool
	monitorCancel   context.CancelFunc
	closeOnce       sync.Once
	closeErr        error
}

// Open 从唯一的基础设施配置建立专用连接和只读连接池，并尝试取得固定锁。
//
// Open 不启动存活检查；调用方必须先执行 Migrate，再调用 StartMonitoring。
func Open(
	ctx context.Context,
	postgresConfig infra.PostgreSQLConfig,
	runtimeConfig infra.RuntimeHostConfig,
	migrations []migration.Migration,
) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("open PostgreSQL runtime: context is required")
	}
	if runtimeConfig.LockCheckInterval <= 0 || runtimeConfig.LockCheckTimeout <= 0 ||
		runtimeConfig.LockCheckTimeout > runtimeConfig.LockCheckInterval {
		return nil, errors.New("open PostgreSQL runtime: lock check configuration is invalid")
	}

	lockConfig, err := pgx.ParseConfig(postgresConfig.RuntimeWriteDSN.Value())
	if err != nil {
		return nil, errors.New("open PostgreSQL runtime: parse connection settings")
	}
	lockConn, err := pgx.ConnectConfig(ctx, lockConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL runtime: connect dedicated connection: %w", err)
	}

	readConfig, err := pgxpool.ParseConfig(postgresConfig.RuntimeReadDSN.Value())
	if err != nil {
		_ = lockConn.Close(ctx)
		return nil, errors.New("open PostgreSQL runtime: parse read pool settings")
	}
	readConfig.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	readConfig.ConnConfig.RuntimeParams["timezone"] = "UTC"
	readConfig.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		identity, err := inspectIdentity(ctx, connection)
		if err != nil {
			return fmt.Errorf("inspect Runtime read identity: %w", err)
		}
		return verifyDirectLoginIdentity("Runtime read", identity)
	}
	readPool, err := pgxpool.NewWithConfig(ctx, readConfig)
	if err != nil {
		_ = lockConn.Close(ctx)
		return nil, fmt.Errorf("open PostgreSQL runtime: create read pool: %w", err)
	}
	if err := readPool.Ping(ctx); err != nil {
		readPool.Close()
		_ = lockConn.Close(ctx)
		return nil, fmt.Errorf("open PostgreSQL runtime: ping read pool: %w", err)
	}

	var acquired bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&acquired); err != nil {
		readPool.Close()
		_ = lockConn.Close(ctx)
		return nil, fmt.Errorf("open PostgreSQL runtime: acquire advisory lock: %w", err)
	}
	if !acquired {
		readPool.Close()
		_ = lockConn.Close(ctx)
		return nil, ErrAdvisoryLockUnavailable
	}

	migrationConfig, err := pgx.ParseConfig(postgresConfig.MigrationDSN.Value())
	if err != nil {
		readPool.Close()
		_, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		_ = lockConn.Close(ctx)
		return nil, errors.New("open PostgreSQL runtime: parse migration connection settings")
	}
	migrationConn, err := pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		readPool.Close()
		_, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		_ = lockConn.Close(ctx)
		return nil, fmt.Errorf("open PostgreSQL runtime: connect migration connection: %w", err)
	}
	if err := verifySeparatedIdentities(ctx, lockConn, migrationConn, readPool); err != nil {
		_ = migrationConn.Close(ctx)
		readPool.Close()
		_, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		_ = lockConn.Close(ctx)
		return nil, fmt.Errorf("open PostgreSQL runtime: %w", err)
	}

	runner, err := migration.NewRunner(migrationConn, migrations)
	if err != nil {
		_ = migrationConn.Close(ctx)
		readPool.Close()
		_, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		_ = lockConn.Close(ctx)
		return nil, fmt.Errorf("open PostgreSQL runtime: create migration runner: %w", err)
	}

	runtime := &Runtime{
		lockConn:          lockConn,
		migrationConn:     migrationConn,
		migrator:          runner,
		lockCheckInterval: runtimeConfig.LockCheckInterval,
		lockCheckTimeout:  runtimeConfig.LockCheckTimeout,
		gate:              make(chan struct{}, 1),
		failure:           make(chan error, 1),
		monitorDone:       make(chan struct{}),
		state:             stateOpen,
		acceptingWrites:   true,
	}
	runtime.gate <- struct{}{}
	runtime.reader = newReadPool(readPool)
	runtime.clock = &DatabaseClock{reader: runtime.reader}
	runtime.executor = &writeExecutor{runtime: runtime}

	return runtime, nil
}

// Migrate 在 Runtime 持锁期间通过专用 Migration 连接运行已装配的版本化 Migration。
func (r *Runtime) Migrate(ctx context.Context) (resultErr error) {
	if r == nil || r.migrator == nil {
		return errors.New("migrate PostgreSQL runtime: runtime is not initialized")
	}
	if ctx == nil {
		return errors.New("migrate PostgreSQL runtime: context is required")
	}
	if err := r.acquireGate(ctx); err != nil {
		return fmt.Errorf("migrate PostgreSQL runtime: %w", err)
	}
	defer r.releaseGate()
	defer func() {
		if resultErr != nil {
			r.markStartupFailed()
		}
	}()
	defer func() {
		if err := r.closeMigrationConnection(context.WithoutCancel(ctx)); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()

	if err := r.requireState(stateOpen); err != nil {
		return fmt.Errorf("migrate PostgreSQL runtime: %w", err)
	}
	if err := r.migrator.Migrate(ctx); err != nil {
		r.observeConnectionFailure(err)
		return err
	}
	if err := verifyReadIdentityPrivileges(ctx, r.reader); err != nil {
		return fmt.Errorf("verify PostgreSQL read identity after migrations: %w", err)
	}
	return nil
}

func (r *Runtime) markStartupFailed() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.state == stateOpen {
		r.state = stateStartupFailed
	}
	r.acceptingWrites = false
}

// StartMonitoring 启动持锁连接存活检查；每个 Runtime 只能调用一次。
func (r *Runtime) StartMonitoring() error {
	if r == nil {
		return errors.New("start PostgreSQL runtime monitoring: runtime is required")
	}

	r.stateMu.Lock()
	if r.state != stateOpen || !r.acceptingWrites {
		r.stateMu.Unlock()
		return errors.New("start PostgreSQL runtime monitoring: runtime is not open")
	}
	if r.lockConnectionClosed() {
		r.state = stateLost
		r.stateMu.Unlock()
		r.reportFailure(errors.New("start monitoring: dedicated connection is closed"))
		return ErrLockConnectionLost
	}
	monitorCtx, cancel := context.WithCancel(context.Background())
	r.monitorCancel = cancel
	r.state = stateMonitoring
	r.stateMu.Unlock()

	go r.monitor(monitorCtx)
	return nil
}

// Done 在持锁连接失效时返回不可恢复的运行错误。
func (r *Runtime) Done() <-chan error {
	if r == nil {
		return nil
	}
	return r.failure
}

// ReadPool 返回不暴露写入或底层连接获取方法的普通查询池。
func (r *Runtime) ReadPool() *ReadPool {
	if r == nil {
		return nil
	}
	return r.reader
}

// Clock 返回使用普通只读池查询 PostgreSQL 权威时间的时钟。
func (r *Runtime) Clock() *DatabaseClock {
	if r == nil {
		return nil
	}
	return r.clock
}

// WriteExecutor 返回绑定持锁连接、且只暴露共享 Port 的唯一串行写事务入口。
func (r *Runtime) WriteExecutor() contracts.RuntimeWriteExecutor {
	if r == nil {
		return nil
	}
	return r.executor
}

// StopAcceptingWrites 立即封闭 Runtime 写事务准入，但不等待活动事务或释放数据库资源。
//
// 已经取得串行 gate 并通过最终准入检查的事务可以继续；重复调用是安全的。
func (r *Runtime) StopAcceptingWrites() {
	if r == nil {
		return
	}
	r.stateMu.Lock()
	r.acceptingWrites = false
	r.stateMu.Unlock()
}

// Close 停止监控和新写入，等待当前操作，并在 Context 超时后强制释放全部数据库资源。
//
// 第一次调用决定关闭宽限期和最终结果；并发及后续调用返回同一个最终结果。
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("close PostgreSQL runtime: context is required")
	}

	r.closeOnce.Do(func() {
		r.closeErr = r.close(ctx)
	})

	return r.closeErr
}

func (r *Runtime) close(ctx context.Context) error {
	// Read admission is sealed synchronously before any graceful waiting starts.
	r.reader.stopAccepting()
	previousState, cancelMonitoring := r.beginClosing()
	if cancelMonitoring != nil {
		cancelMonitoring()
	}

	readPoolDone := make(chan error, 1)
	go func() {
		readPoolDone <- r.reader.shutdown(ctx)
	}()

	var closeErr error
	gateAcquired := false
	if err := r.acquireGate(ctx); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("wait for active PostgreSQL operation: %w", err))
	} else {
		gateAcquired = true
	}

	if err := r.closeMigrationConnection(ctx); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	if gateAcquired {
		closeErr = errors.Join(closeErr, r.releaseLockAndCloseConnection(ctx))
		r.releaseGate()
	} else {
		closeErr = errors.Join(closeErr, r.forceCloseLockConnection(ctx))
	}

	if previousState == stateMonitoring {
		<-r.monitorDone
	}
	closeErr = errors.Join(closeErr, <-readPoolDone)

	r.stateMu.Lock()
	r.state = stateClosed
	r.stateMu.Unlock()
	return closeErr
}

func (r *Runtime) beginClosing() (lifecycleState, context.CancelFunc) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	previousState := r.state
	r.acceptingWrites = false
	r.state = stateClosing
	return previousState, r.monitorCancel
}

func (r *Runtime) releaseLockAndCloseConnection(ctx context.Context) error {
	r.lockConnUse.Lock()
	defer r.lockConnUse.Unlock()

	var closeErr error
	if !r.lockConn.IsClosed() {
		var unlocked bool
		if err := r.lockConn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey).Scan(&unlocked); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("release PostgreSQL advisory lock: %w", err))
		} else if !unlocked {
			closeErr = errors.Join(closeErr, errors.New("release PostgreSQL advisory lock: lock was not held"))
		}
	}
	if err := r.lockConn.Close(ctx); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close PostgreSQL dedicated connection: %w", err))
	}
	return closeErr
}

func (r *Runtime) forceCloseLockConnection(ctx context.Context) error {
	var closeErr error
	if err := r.lockConn.PgConn().Conn().Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		closeErr = errors.Join(closeErr, fmt.Errorf("force close PostgreSQL dedicated network connection: %w", err))
	}

	r.lockConnUse.Lock()
	defer r.lockConnUse.Unlock()
	if err := r.lockConn.Close(ctx); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close PostgreSQL dedicated connection: %w", err))
	}
	return closeErr
}

func (r *Runtime) closeMigrationConnection(ctx context.Context) error {
	r.stateMu.Lock()
	connection := r.migrationConn
	r.migrationConn = nil
	r.stateMu.Unlock()
	if connection == nil {
		return nil
	}
	if err := connection.Close(ctx); err != nil {
		return fmt.Errorf("close PostgreSQL migration connection: %w", err)
	}
	return nil
}

type databaseIdentity struct {
	sessionUser string
	currentUser string
	database    string
	superuser   bool
	createRole  bool
	createDB    bool
	replicate   bool
	bypassRLS   bool
}

func verifySeparatedIdentities(
	ctx context.Context,
	writeConnection *pgx.Conn,
	migrationConnection *pgx.Conn,
	readPool *pgxpool.Pool,
) error {
	writeIdentity, err := inspectIdentity(ctx, writeConnection)
	if err != nil {
		return fmt.Errorf("inspect Runtime write identity: %w", err)
	}
	migrationIdentity, err := inspectIdentity(ctx, migrationConnection)
	if err != nil {
		return fmt.Errorf("inspect Migration identity: %w", err)
	}
	readIdentity, err := inspectPoolIdentity(ctx, readPool)
	if err != nil {
		return fmt.Errorf("inspect Runtime read identity: %w", err)
	}
	for _, identity := range []struct {
		label string
		value databaseIdentity
	}{
		{label: "Runtime write", value: writeIdentity},
		{label: "Migration", value: migrationIdentity},
		{label: "Runtime read", value: readIdentity},
	} {
		if err := verifyDirectLoginIdentity(identity.label, identity.value); err != nil {
			return err
		}
	}
	if writeIdentity.database != migrationIdentity.database || writeIdentity.database != readIdentity.database {
		return fmt.Errorf("%w: identities target different databases", ErrUnsafeDatabaseIdentity)
	}
	if writeIdentity.sessionUser == migrationIdentity.sessionUser ||
		writeIdentity.sessionUser == readIdentity.sessionUser ||
		migrationIdentity.sessionUser == readIdentity.sessionUser {
		return fmt.Errorf("%w: Migration, Runtime write and Runtime read session users must be distinct", ErrUnsafeDatabaseIdentity)
	}
	if readIdentity.superuser || readIdentity.createRole || readIdentity.createDB || readIdentity.replicate || readIdentity.bypassRLS {
		return fmt.Errorf("%w: Runtime read user has elevated role attributes", ErrUnsafeDatabaseIdentity)
	}

	var hasMembership bool
	if err := readPool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_auth_members memberships
    JOIN pg_roles member_role ON member_role.oid = memberships.member
    WHERE member_role.rolname = session_user
)`).Scan(&hasMembership); err != nil {
		return fmt.Errorf("inspect Runtime read role memberships: %w", err)
	}
	if hasMembership {
		return fmt.Errorf("%w: Runtime read user must not be a member of another role", ErrUnsafeDatabaseIdentity)
	}
	return nil
}

func verifyDirectLoginIdentity(label string, identity databaseIdentity) error {
	if identity.sessionUser != identity.currentUser {
		return fmt.Errorf(
			"%w: %s connection session user must equal current user",
			ErrUnsafeDatabaseIdentity,
			label,
		)
	}
	return nil
}

func inspectIdentity(ctx context.Context, connection *pgx.Conn) (databaseIdentity, error) {
	var identity databaseIdentity
	err := connection.QueryRow(ctx, `
SELECT session_user, current_user, current_database(),
       roles.rolsuper, roles.rolcreaterole, roles.rolcreatedb,
       roles.rolreplication, roles.rolbypassrls
FROM pg_roles roles
WHERE roles.rolname = session_user`).Scan(
		&identity.sessionUser,
		&identity.currentUser,
		&identity.database,
		&identity.superuser,
		&identity.createRole,
		&identity.createDB,
		&identity.replicate,
		&identity.bypassRLS,
	)
	return identity, err
}

func inspectPoolIdentity(ctx context.Context, pool *pgxpool.Pool) (databaseIdentity, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return databaseIdentity{}, err
	}
	defer connection.Release()
	return inspectIdentity(ctx, connection.Conn())
}

func verifyReadIdentityPrivileges(ctx context.Context, reader *ReadPool) error {
	var unsafe bool
	err := reader.QueryRow(ctx, `
SELECT
    EXISTS (
        SELECT 1
        FROM pg_namespace schemas
        WHERE schemas.nspname NOT IN ('pg_catalog', 'information_schema')
          AND schemas.nspname !~ '^pg_toast'
          AND (
              schemas.nspowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
              OR has_schema_privilege(current_user, schemas.oid, 'CREATE')
          )
    )
    OR EXISTS (
        SELECT 1
        FROM pg_class relations
        JOIN pg_namespace schemas ON schemas.oid = relations.relnamespace
        WHERE schemas.nspname NOT IN ('pg_catalog', 'information_schema')
          AND schemas.nspname !~ '^pg_toast'
          AND relations.relkind IN ('r', 'p', 'v', 'm', 'f')
          AND (
              relations.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
              OR has_table_privilege(current_user, relations.oid, 'INSERT')
              OR has_table_privilege(current_user, relations.oid, 'UPDATE')
              OR has_table_privilege(current_user, relations.oid, 'DELETE')
              OR has_table_privilege(current_user, relations.oid, 'TRUNCATE')
              OR has_table_privilege(current_user, relations.oid, 'REFERENCES')
              OR has_table_privilege(current_user, relations.oid, 'TRIGGER')
          )
    )
    OR EXISTS (
        SELECT 1
        FROM pg_attribute columns
        JOIN pg_class relations ON relations.oid = columns.attrelid
        JOIN pg_namespace schemas ON schemas.oid = relations.relnamespace
        WHERE schemas.nspname NOT IN ('pg_catalog', 'information_schema')
          AND schemas.nspname !~ '^pg_toast'
          AND relations.relkind IN ('r', 'p', 'v', 'm', 'f')
          AND columns.attnum > 0
          AND NOT columns.attisdropped
          AND (
              has_column_privilege(current_user, relations.oid, columns.attnum, 'INSERT')
              OR has_column_privilege(current_user, relations.oid, columns.attnum, 'UPDATE')
              OR has_column_privilege(current_user, relations.oid, columns.attnum, 'REFERENCES')
          )
    )
    OR EXISTS (
        SELECT 1
        FROM pg_class sequences
        JOIN pg_namespace schemas ON schemas.oid = sequences.relnamespace
        WHERE schemas.nspname NOT IN ('pg_catalog', 'information_schema')
          AND schemas.nspname !~ '^pg_toast'
          AND sequences.relkind = 'S'
          AND (
              sequences.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
              OR has_sequence_privilege(current_user, sequences.oid, 'USAGE')
              OR has_sequence_privilege(current_user, sequences.oid, 'UPDATE')
          )
    )
    OR EXISTS (
        SELECT 1
        FROM pg_proc functions
        JOIN pg_namespace schemas ON schemas.oid = functions.pronamespace
        WHERE schemas.nspname NOT IN ('pg_catalog', 'information_schema')
          AND schemas.nspname !~ '^pg_toast'
          AND functions.prosecdef
          AND has_function_privilege(current_user, functions.oid, 'EXECUTE')
    )`).Scan(&unsafe)
	if err != nil {
		return fmt.Errorf("inspect Runtime read privileges: %w", err)
	}
	if unsafe {
		return fmt.Errorf("%w: Runtime read user owns objects or has write-capable privileges", ErrUnsafeDatabaseIdentity)
	}
	return nil
}

func (r *Runtime) monitor(ctx context.Context) {
	defer close(r.monitorDone)
	ticker := time.NewTicker(r.lockCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.checkLockConnection(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					r.markLost(err)
				}
				return
			}
		}
	}
}

func (r *Runtime) checkLockConnection(monitorCtx context.Context) error {
	if err := r.acquireGate(monitorCtx); err != nil {
		return err
	}
	defer r.releaseGate()

	checkCtx, cancel := context.WithTimeout(monitorCtx, r.lockCheckTimeout)
	defer cancel()

	r.lockConnUse.Lock()
	defer r.lockConnUse.Unlock()
	if r.lockConn.IsClosed() {
		return errors.New("dedicated connection is closed")
	}
	var one int
	if err := r.lockConn.QueryRow(checkCtx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("check dedicated connection: %w", err)
	}
	if one != 1 {
		return errors.New("check dedicated connection: unexpected result")
	}
	return nil
}

func (r *Runtime) markLost(cause error) {
	r.stateMu.Lock()
	if r.state == stateClosing || r.state == stateClosed || r.state == stateLost {
		r.stateMu.Unlock()
		return
	}
	r.state = stateLost
	r.acceptingWrites = false
	r.stateMu.Unlock()
	r.reportFailure(cause)
}

func (r *Runtime) reportFailure(cause error) {
	err := fmt.Errorf("%w: %v", ErrLockConnectionLost, cause)
	select {
	case r.failure <- err:
	default:
	}
}

func (r *Runtime) observeConnectionFailure(cause error) {
	if cause != nil && r.lockConnectionClosed() {
		r.markLost(cause)
	}
}

func (r *Runtime) lockConnectionClosed() bool {
	r.lockConnUse.Lock()
	defer r.lockConnUse.Unlock()
	return r.lockConn.IsClosed()
}

func (r *Runtime) acquireGate(ctx context.Context) error {
	r.gateMu.Lock()
	r.gateWaiters++
	r.gateMu.Unlock()

	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-r.gate:
	}
	r.gateMu.Lock()
	r.gateWaiters--
	r.gateMu.Unlock()
	return err
}

// tryAcquireGate 将普通写等待者优先级和非阻塞 gate 获取放在同一临界区判断。
func (r *Runtime) tryAcquireGate() bool {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.gateWaiters != 0 {
		return false
	}
	select {
	case <-r.gate:
		return true
	default:
		return false
	}
}

func (r *Runtime) releaseGate() {
	r.gate <- struct{}{}
}

// admitWrite 在调用方持有 gate 时完成最终准入检查；成功返回即表示事务已经被接纳。
func (r *Runtime) admitWrite() error {
	if err := r.requireAcceptingWrites(); err != nil {
		return err
	}
	if r.lockConnectionClosed() {
		r.markLost(errors.New("dedicated connection is closed"))
		return ErrWriteUnavailable
	}
	return nil
}

func (r *Runtime) requireAcceptingWrites() error {
	r.stateMu.RLock()
	state := r.state
	acceptingWrites := r.acceptingWrites
	r.stateMu.RUnlock()
	if !acceptingWrites || (state != stateOpen && state != stateMonitoring) {
		return ErrWriteUnavailable
	}
	return nil
}

func (r *Runtime) requireState(expected lifecycleState) error {
	r.stateMu.RLock()
	state := r.state
	r.stateMu.RUnlock()
	if state != expected {
		return ErrWriteUnavailable
	}
	if r.lockConnectionClosed() {
		r.markLost(errors.New("dedicated connection is closed"))
		return ErrWriteUnavailable
	}
	return nil
}

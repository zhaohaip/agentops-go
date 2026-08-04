# PostgreSQL 运行身份

AgentOps Runtime 必须使用同一 Database 中三个互不相同的 `LOGIN` 角色：

| 身份 | 必要权限 | 禁止权限 |
|---|---|---|
| `agentops_migration` | Database/Schema DDL；执行版本化 Migration | 运行期查询入口不得使用 |
| `agentops_runtime_write` | `CONNECT`、业务 Schema `USAGE`、业务表 DML、必要 Sequence 权限 | DDL、角色管理、超级用户 |
| `agentops_runtime_read` | `CONNECT`、业务 Schema `USAGE`、业务表 `SELECT` | 表 DML、Sequence `USAGE/UPDATE`、Schema `CREATE`、角色继承、超级用户 |

三个角色均应使用 `NOINHERIT`；只读角色不得加入任何其他角色。配置项分别为
`migration_dsn`、`runtime_write_dsn` 和 `runtime_read_dsn`。

推荐初始化轮廓如下，密码应由部署环境的 Secret 管理：

```sql
CREATE ROLE agentops_migration LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE agentops_runtime_write LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE agentops_runtime_read LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;

ALTER DATABASE zhp OWNER TO agentops_migration;
REVOKE CONNECT ON DATABASE zhp FROM PUBLIC;
GRANT CONNECT ON DATABASE zhp TO agentops_migration, agentops_runtime_write, agentops_runtime_read;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO agentops_runtime_write, agentops_runtime_read;
REVOKE CREATE ON SCHEMA public FROM agentops_runtime_read;
REVOKE TEMP, CREATE ON DATABASE zhp FROM agentops_runtime_read;
```

每个领域 Migration 创建表后必须在同一 Migration 中授予：

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON <table> TO agentops_runtime_write;
GRANT SELECT ON <table> TO agentops_runtime_read;
```

Identity/Serial Sequence 只向写身份授予 `USAGE, SELECT, UPDATE`；读身份最多授予
`SELECT`。不得向只读身份授予 Security Definer 写函数的 `EXECUTE`。Runtime 在
Migration 完成、HTTP 启动前会检查只读身份的角色属性、角色成员关系、对象所有权、
Schema CREATE、表写权限、Sequence 写权限和可执行的 Security Definer 函数；不安全
配置会使启动失败。

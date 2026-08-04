本文件用于记录项目已经确定采用的技术栈、引入阶段及其使用规范。

仅记录已冻结选型或已实际使用的技术；未进入当前实施阶段的选型必须明确标注引入阶段。

新增技术时，应补充：

* 技术名称；
* 使用范围；
* 使用约束；
* 禁止事项。

不记录技术选型原因、使用教程或未确定的候选方案。

---

# HTTP Framework

## Gin

### 引入阶段

Gin 已冻结为 Phase 1 HTTP API Handler 与 Router 的选型。Phase 0 只提供基于 `net/http.Server` 的监听、启停和 Handler 生命周期边界，不实现业务 Router 或 Handler，因此本阶段不引入 Gin 依赖。

### 使用范围

* HTTP Server
* Router
* Middleware

### 使用规范

* 统一使用 `github.com/gin-gonic/gin`。
* `gin.Context` 仅允许在 HTTP 层使用。
* HTTP Handler 不应包含业务逻辑，应调用应用层完成业务处理。
* Middleware 仅处理 HTTP 相关能力，例如日志、认证、请求追踪、限流等。
* 路由应统一注册，不允许分散初始化。

### 标准库使用

允许使用 `net/http`：

* 创建 `http.Server`
* 配置超时
* Server 启停
* 使用 HTTP 状态码

不得使用 `net/http` 单独实现 Router 或 Handler，与 Gin 并存形成两套路由体系。

---

# 文档维护

仅在技术栈确定后更新本文件。

技术尚未确定时，不提前加入本文件。

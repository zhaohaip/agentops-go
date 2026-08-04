## Go 项目目录规范

1. 程序入口放在 cmd/<app>/main.go。
2. 业务代码默认放在 internal/，禁止无理由放入 pkg/。
3. pkg/ 只存放可被其他项目复用且 API 稳定的公共包。
4. 中大型项目优先按业务模块组织代码，不按 handler/service/repository 全局平铺。
5. main.go 只负责配置加载、依赖组装和程序启动，不包含业务逻辑。
6. API 定义放在 api/，接口实现放在 internal/。
7. 配置模板放在 configs/，配置读取逻辑放在 internal/config/。
8. 数据库迁移放在 migrations/。
9. 部署文件放在 deployments/。
10. 单元测试与源码同目录，集成测试和 E2E 测试放在 test/。
11. 禁止创建含义模糊的 common、utils、helper、misc 等杂物包。
12. 不提前创建无实际代码和明确职责的目录。
13. 新增目录前必须明确其业务职责和依赖边界。
14. 修改目录结构时不得进行与当前任务无关的大范围迁移。

# Git 提交前检查与提交规范

## 1. 目的

本规范用于指导 Codex 在开发任务完成且代码 Review 通过后，执行：

1. 工作区检查；
2. 提交范围确认；
3. 敏感信息检查；
4. 提交前验证；
5. 文件暂存；
6. 本地 Git commit；
7. 提交结果确认。

本规范只负责提交前检查和代码提交，不代替代码 Review。

## 2. 适用范围

适用于项目中所有：

* 功能开发；
* Bug 修复；
* Review 问题修复；
* 测试补充；
* 设计文档同步；
* 阶段性开发成果。

执行前必须明确本次任务或阶段的提交范围。

## 3. 基本原则

1. 只提交与当前任务有关的修改。
2. 保留用户已有的无关修改。
3. 不擅自丢弃、覆盖或回滚文件。
4. 不把本地配置、凭据、日志和构建产物提交到仓库。
5. 提交前必须执行项目规定的验证。
6. 验证失败时不得继续提交。
7. 默认只创建本地 commit，不执行 `git push`。
8. 除非用户明确要求，否则不修改已有 commit，不执行 `commit --amend`。
9. 不为通过验证而擅自修改业务逻辑或扩大任务范围。
10. 不重复进行完整代码 Review，但发现明显的提交阻塞问题时必须停止。

## 4. 开始前阅读

执行提交前，先阅读并遵守：

* 项目根目录及相关目录中的 `AGENTS.md`；
* 项目开发规范；
* 项目 Git 提交规范；
* 当前任务对应的需求、设计或开发计划；
* 仓库近期 Git commit 的消息格式。

如果文件路径或名称发生变化，应在仓库中查找对应文件。

## 5. 工作区检查

先执行：

```bash
git status --short
git diff
git diff --cached
git log -10 --oneline
git branch --show-current
```

确认以下内容：

1. 当前修改是否属于本次任务；
2. 是否存在与本次任务无关的用户修改；
3. 是否存在未跟踪但应提交的文件；
4. 是否存在误删除文件；
5. 是否存在已经暂存但不属于本次任务的文件；
6. 是否遗漏本次任务要求同步的测试或文档；
7. 当前分支是否正确；
8. 当前提交是否会形成完整、可理解的功能单元。

如果无法判断某个文件是否属于本次提交，停止提交并向用户说明，不得自行猜测。

## 6. 禁止提交的内容

检查暂存内容中不得包含：

* `.env`、`.env.test` 等本地环境文件；
* 数据库密码、Token、API Key、Cookie、私钥；
* 包含真实凭据的 DSN 或连接字符串；
* IDE 本地配置；
* 本地日志；
* 临时调试文件；
* 测试运行产生的临时文件；
* 编译产物；
* 覆盖率文件；
* 操作系统生成的无关文件；
* 与当前任务无关的修改。

示例检查命令：

```bash
git diff --check
git status --short
```

必要时可以在本次待提交内容中搜索常见敏感字段，但不得在输出中泄露发现的凭据内容。

如果发现敏感信息，立即停止提交，只报告文件位置和问题类型。

## 7. 提交前验证

### 7.1 优先使用项目统一入口

如果项目提供 Makefile、Taskfile 或专用验证脚本，优先执行项目规定的入口，例如：

```bash
make test
make test-phase0
make lint
make verify
```

实际命令以项目现有配置为准。

### 7.2 Go 项目基础验证

Go 项目至少执行：

```bash
go test ./... -count=1
go test -race ./... -count=1
go build ./...
go vet ./...
```

如果已经安装相关工具，还应执行：

```bash
gofmt -l .
goimports -l .
golangci-lint run ./...
```

格式检查有输出时，说明仍有文件未正确格式化。

### 7.3 集成测试

如果本次修改涉及 PostgreSQL、外部服务或其他基础设施：

1. 必须加载项目规定的测试环境；
2. 必须确认集成测试实际执行；
3. 不能把测试全部 Skip 当作通过；
4. 不能把编译通过当作集成测试通过；
5. 测试结束后检查临时 Database、Schema、Role、表或其他对象是否清理；
6. 本地测试凭据不得加入暂存区。

### 7.4 结果分类

每项验证必须明确记录为：

* `PASS`：已经执行并通过；
* `FAIL`：已经执行但失败；
* `BLOCKED`：因为环境或依赖缺失无法执行；
* `SKIPPED`：满足明确的跳过条件而未运行。

不得将 `BLOCKED` 或 `SKIPPED` 表述为验证通过。

如果必要测试、构建或静态检查失败，停止提交并报告失败证据。

如果工具未安装，应如实记录，不得伪造结果，也不得未经允许修改项目来规避检查。

## 8. 文件暂存

验证通过后，只暂存属于当前任务的文件。

优先使用明确的文件路径：

```bash
git add <file-or-directory>
```

不要默认使用：

```bash
git add .
git add -A
```

只有确认当前工作区的全部修改都属于本次任务时，才允许整体暂存。

暂存后必须重新检查：

```bash
git diff --cached --stat
git diff --cached
git diff --cached --check
```

确认：

1. 暂存文件全部属于当前任务；
2. 没有遗漏必须提交的实现、测试或文档；
3. 没有混入无关修改；
4. 没有敏感信息；
5. 没有异常大文件；
6. 没有意外删除；
7. 暂存内容能够形成完整提交。

## 9. Commit Message

根据仓库现有风格生成提交信息，不机械照抄示例。

如果项目采用 Conventional Commits，可以使用：

```text
feat: complete task runtime foundation
fix: close runtime shutdown findings
test: add PostgreSQL lifecycle coverage
docs: update development implementation plan
refactor: simplify runtime lifecycle management
```

提交信息应满足：

* 准确描述本次提交的主要目的；
* 使用一个主要类型；
* 避免使用“update code”“modify files”等模糊描述；
* 不夸大实际完成范围；
* 不在提交信息中写入敏感数据；
* 与仓库已有提交语言和格式保持一致。

如果一次提交包含功能、测试和配套文档，通常以主要业务变更作为 commit 类型，不需要为每类文件分别创建 commit。

## 10. 创建本地提交

确认暂存内容后执行：

```bash
git commit -m "<commit message>"
```

默认禁止执行：

```bash
git commit --amend
git push
git push --force
git push --force-with-lease
git reset --hard
git clean
git checkout -- <file>
git restore <file>
```

除非用户明确授权，否则不得修改历史、推送远程或丢弃用户修改。

## 11. 提交后确认

提交完成后执行：

```bash
git status --short
git log -1 --oneline
git show --stat --oneline HEAD
```

必要时执行：

```bash
git diff HEAD^ HEAD
```

确认：

1. commit 已成功创建；
2. commit 只包含预期文件；
3. 当前任务的文件没有遗漏；
4. 无关的未提交修改仍然保留；
5. 工作区剩余状态明确；
6. commit 尚未被自动推送到远程。

## 12. 必须停止提交的情况

出现以下任一情况时，停止操作并向用户报告：

* 必要测试、构建或静态检查失败；
* 集成测试应该执行但实际全部 Skip；
* 发现敏感信息；
* 当前分支无法确认；
* 修改文件归属无法确认；
* 暂存区混入无关修改；
* 存在无法解释的文件删除；
* 发现可能覆盖或丢失用户修改的风险；
* Git 出现冲突；
* 提交内容与已经通过 Review 的范围明显不一致；
* 需要修改业务代码才能完成提交；
* 需要执行 amend、rebase、强制推送等历史修改操作。

停止后不得自行丢弃修改或扩大任务范围。

## 13. 完成后输出

完成后按以下格式报告：

### 提交结果

* 提交状态：成功／未提交
* 当前分支：
* Commit Hash：
* Commit Message：
* 是否已经 Push：否，除非用户明确要求

### 提交内容

列出本次提交包含的主要文件和修改内容。

### 验证结果

| 验证项           | 状态                        | 结果说明   |
| ------------- | ------------------------- | ------ |
| 单元测试          | PASS/FAIL/BLOCKED/SKIPPED | 实际结果   |
| Race 测试       | PASS/FAIL/BLOCKED/SKIPPED | 实际结果   |
| 构建            | PASS/FAIL/BLOCKED/SKIPPED | 实际结果   |
| go vet        | PASS/FAIL/BLOCKED/SKIPPED | 实际结果   |
| golangci-lint | PASS/FAIL/BLOCKED/SKIPPED | 实际结果   |
| 集成测试          | PASS/FAIL/BLOCKED/SKIPPED | 是否真实执行 |

### 工作区状态

说明：

* 提交后是否仍有未提交文件；
* 剩余文件是否属于无关用户修改；
* 是否存在未执行的验证及原因；
* 是否存在尚未推送的本地 commit。

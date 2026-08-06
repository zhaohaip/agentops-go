执行测试时必须遵守：

## Testing Rules

- 遵循最小验证原则，仅测试本次修改涉及的 package 或 TestCase。
- 默认禁止执行 `go test ./...`，仅在用户明确要求或最终验收时执行。
- 测试必须设置超时（如 `-timeout=5m`），限制并发（如 `-p 2`）。
- 同一时间仅允许一个测试任务运行，不得并行执行多个测试或静态检查。
- 测试结束前必须释放所有资源，确保无残留进程、线程、Goroutine 或后台服务。
- 若出现资源异常（如 `fork failed`、`resource temporarily unavailable`），立即停止测试并优先恢复环境，不得继续启动新的测试任务。
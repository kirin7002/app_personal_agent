# H02 Hermes Runs 和轮次编排

`vehicle/turns.Service` 接收可信音频轮次。调试文本通过 `turns.UtteranceHandler` 挂载在 `POST /voice/utterances`，使用相同认证、轮次和播报逻辑。

接线时注入 `contract.PendingReader`、`contract.CapabilityFinder`、`contract.ProgressReader` 和 `runs.Client`。使用 `turns.NewMemoryStore()` 运行单进程模拟。MySQL 部署执行 `vehicle/turns/turns.sql` 后注入 `turns.MySQLStore{DB: db}`。同一 DB 池至少为当前并发轮次保留各一条协调连接，并为读写任务和 MCP 回调留出连接。会话与车辆作用域锁覆盖一轮 HTTP 交互，使用连接级锁。单车端按接收顺序提交，跨实例同作用域通过 MySQL 互斥。

H04 在自己的业务事务内调用 `turns.LockWritableTurn(ctx, tx, metadata, now)`，复用 H02 取消轮次锁定的行。继续在同一事务内检查一轮一次写入和候选版本，然后提交任务及 outbox。H02 在 Run 受理前注册作用域、快照和句柄，支持先于 HTTP 202 的工具回调。终态关闭轮次写入。使用 `voice_turn_id → request_id → run_id → task_id → command_id` 关联应用事件。句柄通过认证传输头发送。

Hermes 接线以 H01 锁定部署为准。客户端按默认保守模式发送一次受理请求。H01 确认部署支持受理幂等后，同时打开 `runs.Client.RetryAdmission` 和 `turns.Service.RetryAdmission`，仅使用持久化的同一 JSON、幂等键和句柄恢复。Run 流中断后查询状态并重新订阅旧 Run。车辆实际状态从 Go 的进度读取接口取得，车辆确认由独立结果事件播报。[Hermes Runs API](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server#runs-api-streaming-friendly-alternative)

```sh
go test -race ./vehicle/... -v
go test -race ./...
go vet ./...
git diff --check
```

测试通过。覆盖受理超时稳定重试、SSE 断线与重复序号、提前终态轮询、并发转写重放、启动前 MCP 轮次可见、零召回补入待办 Schema、取消先于登记、收到失败终态后保留业务进度。真 Hermes 和 MySQL 的集成状态由 H01/H03 及 H07 联验。


SQLite 在独立数据库连接之间通过 `voice_scope_queue` 按登记顺序串行处理同一作用域。等待 Hermes 时数据库保持可写，MCP 可以正常处理工具回调。取消会清除等待记录，进程异常留下的记录最多保留两分钟。正常轮次含等待最多 90 秒，可信轮次有效期受同一截止时间约束。

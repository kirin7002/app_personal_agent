# app_personal_agent

基于 **Go 语音/车控服务 + Hermes 运行时 + 百炼 Qwen Realtime** 的车端个人 Agent（当前处于 Demo 阶段，默认使用内置 SQLite 与模拟车辆通道）。

完整架构、契约、H01–H09 验证报告与优化路线图见 [doc/README.md](./doc/README.md)。

---

## 快速启动 Demo

### 1. 一键启动（Go 服务 + Hermes）

需提前准备好百炼 `DASHSCOPE_API_KEY` 以及 H01 锁定的 Hermes 运行时（`vgit.72c8803` / `mcp==2.3.0`）：

```bash
export DASHSCOPE_API_KEY="<your-dashscope-api-key>"
export HERMES_PYTHON="/path/to/hermes-venv/bin/python"
export HERMES_AGENT_DIR="/path/to/hermes-agent"

./scripts/start-demo.sh
```

脚本会自动：
1. 编译并启动 [cmd/server](./cmd/server)（默认监听 `127.0.0.1:8080` 对外网关、`127.0.0.1:18080` 内部 MCP，使用 `./data/vehicle-demo.sqlite`）；
2. 通过 [hermes/gateway_launcher.py](./hermes/gateway_launcher.py) 拉起隔离配置的 Hermes Gateway（端口 `8642`）；
3. 启动内置 `ExpiryWorker` 与 `executor.Worker`（默认 `SIM_MODE=auto_complete`，模拟车辆自动执行并返回完成回执）。

### 2. 文字接口体验（两轮关窗）

```bash
# 第一轮：缺参进入 pending_reply 并返回追问
curl -N -X POST http://127.0.0.1:8080/voice/utterances \
  -H 'Authorization: Bearer demo-token' \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"car-test","vehicle_id":"sim-car","request_id":"demo-1","text":"把车窗关一下"}'

# 第二轮：补全位置并触发模拟车控下发与回执
curl -N -X POST http://127.0.0.1:8080/voice/utterances \
  -H 'Authorization: Bearer demo-token' \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"car-test","vehicle_id":"sim-car","request_id":"demo-2","text":"左后窗"}'

# 查看模拟车况与结果事件
curl -s 'http://127.0.0.1:8080/debug/state?session_id=car-test&vehicle_id=sim-car' \
  -H 'Authorization: Bearer demo-token'
```

> 若希望手动演示两阶段回执，可在启动时设置 `SIM_MODE=accepted_only`，下发后通过 `POST /debug/receipt`（`{"command_id":"...","status":"completed"}`）注入车辆完成回执。

### 3. 语音接口体验（`car-sim` 模拟车端）

```bash
# 生成测试普通话 16 kHz PCM 样本（macOS）
./scripts/car-audio-samples.sh /tmp/car-samples

# 两轮语音关窗并等待车辆执行完成播报（24 kHz PCM 保存在 /tmp/car-out）
CAR_SIM_TOKEN=demo-token go run ./cmd/car-sim \
  -url ws://127.0.0.1:8080/voice/audio \
  -wait-result \
  -out /tmp/car-out \
  /tmp/car-samples/close-window.pcm \
  /tmp/car-samples/rear-left.pcm
```

---

## 常用配置项（`cmd/server`）

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:8080` | 对外网关地址（`/voice/audio`、`/voice/utterances`、`/healthz`、`/debug/*`） |
| `MCP_LISTEN_ADDR` | `127.0.0.1:18080` | 内部 MCP 地址（`POST /mcp`） |
| `SQLITE_PATH` | `data/vehicle-demo.sqlite` | SQLite 文件路径（`MYSQL_DSN` 为空时使用） |
| `MYSQL_DSN` | 空 | 可选 MySQL DSN，设置后替代 SQLite |
| `HERMES_BASE_URL` | `http://127.0.0.1:8642` | Hermes Runs API 地址 |
| `HERMES_API_KEY` | 必填 | Hermes Runs API Bearer Key（也兼容 `API_SERVER_KEY`） |
| `VEHICLE_MCP_SERVICE_TOKEN` | 必填 | Go MCP 服务间鉴权 Token |
| `DEMO_AUTH_TOKEN` | `demo-token` | Demo 端调用 `/voice/*` 与 `/debug/*` 的 Bearer Token |
| `DASHSCOPE_API_KEY` | 可选 | 配置后启用 `/voice/audio`（未配置时仅启用文字接口） |
| `SIM_MODE` | `auto_complete` | 模拟车辆模式：`auto_complete` / `accepted_only` / `retryable_once` / `rejected` |
| `TTS_MODE` | `auto` | 播报 TTS：`auto`（macOS 用 `say`，其余回退 `tone`）/ `say` / `qwen` / `tone` |

## 运行测试

```bash
go test -race ./... -count=1
go vet ./...
```

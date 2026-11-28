# 文档索引

`doc/` 目录按**架构设计**、**任务交付与验证**、**业务 MCP 协议参考**和**老项目参考**四个子目录归档管理：

```text
doc/
├── README.md        # 文档导航索引（本文件）
├── design/          # 整体设计、实施计划、共享契约与优化路线图
├── tasks/           # H01–H09 各模块交付与联调验证文档
├── mcp/             # 真实业务 MCP 工具清单与 Schema 定义
└── old/             # 同源 Python 老项目架构与 BM25 参考设计
```

## 1. 架构设计与规划（`design/`）

| 文档 | 说明 |
| --- | --- |
| [hermes-vehicle-task-design.md](./design/hermes-vehicle-task-design.md) | Hermes 语音车控任务整体设计与报文定义（第 0–9 节） |
| [hermes-vehicle-task-implementation-plan.md](./design/hermes-vehicle-task-implementation-plan.md) | H00–H09 分阶段实施计划与分工 |
| [vehicle-contract.md](./design/vehicle-contract.md) | H00 共享接口、状态机与车端 WebSocket 协议契约 |
| [optimization-roadmap.md](./design/optimization-roadmap.md) | 架构优化路线图（P0/P1/P2 优先级、反模式红线与验收指标） |
| [web-demo-interface-design.md](./design/web-demo-interface-design.md) | Web Demo 前端页面与后端执行过程 SSE 接口设计 |
| [web-demo-implementation-plan.md](./design/web-demo-implementation-plan.md) | Web Demo 与 SSE 执行过程双 Agent 实施任务拆分（W01–W06） |

## 2. 模块交付与验证报告（`tasks/`）

| 任务卡 | 文档 | 核心内容 |
| --- | --- | --- |
| H01 | [h01-hermes-runtime-report.md](./tasks/h01-hermes-runtime-report.md) | Hermes 运行时版本锁定、非侵入可信轮次绑定与探针实测报告 |
| H02 | [h02-hermes-runs.md](./tasks/h02-hermes-runs.md) | Hermes Runs 客户端与语音/文本轮次编排 |
| H03 | [h03-mysql-tasks.md](./tasks/h03-mysql-tasks.md) | MySQL / SQLite 任务存储、10 分钟过期扫描与命令 Outbox |
| H04 | [h04-mcp-server.md](./tasks/h04-mcp-server.md) | Go Streamable HTTP MCP 服务端、六工具适配与写入护栏 |
| H05 | [h05-executor-receipts.md](./tasks/h05-executor-receipts.md) | 异步执行器、模拟车辆通道、回执对账与完成事件 |
| H06 | [h06-capability-catalog.md](./tasks/h06-capability-catalog.md) | 版本化车辆功能地图、BM25 召回与只读服务 |
| H07 | [h07-end-to-end.md](./tasks/h07-end-to-end.md) | 两轮关窗端到端联调报告、恢复路径补验与延迟基线 |
| H08 | [h08-voice-gateway.md](./tasks/h08-voice-gateway.md) | 车端 WebSocket 音频网关、打断恢复与模拟车端 |
| H09 | [h09-qwen-verification.md](./tasks/h09-qwen-verification.md) | 阿里云百炼 Qwen Realtime 协议适配与实测探针 |

## 3. 业务 MCP 工具参考（`mcp/`）

| 文档 | 说明 |
| --- | --- |
| [业务 MCP 工具清单.md](./mcp/业务%20MCP%20工具清单.md) | 业务图范围下的车控、车况、备车 MCP 及本地/HTTP 工具总览 |
| [MCP 工具 Schema.md](./mcp/MCP%20工具%20Schema.md) | 业务 MCP 工具完整参数类型、必填项与 JSON Schema |

## 4. 老项目参考设计（`old/`）

| 文档 | 说明 |
| --- | --- |
| [personalagetn架构.md](./old/personalagetn架构.md) | 同源车端个人 Agent（Python 版）系统全景、模块职责与场景时序 |
| [BM25.md](./old/BM25.md) | 94 个车端协议工具的 6 字段加权 BM25 与精排检索设计 |

# Web Demo 前端页面与语音/SSE 执行过程验收报告（W04–W06）

> 对应设计文档：[web-demo-interface-design.md](../design/web-demo-interface-design.md) · [web-demo-implementation-plan.md](../design/web-demo-implementation-plan.md)
> 负责范围：Agent B（W04 页面布局与样式、W05 双 SSE 过程渲染与状态机、W06 浏览器 PTT 语音客户端与端到端验证）

---

## 1. 交付文件清单

| 任务卡 | 交付文件 | 核心能力 |
| --- | --- | --- |
| **W04** | [index.html](../../cmd/server/web/index.html)、[style.css](../../cmd/server/web/style.css) | 零构建依赖三栏响应式页面（参考 `personal_agent` 低饱和浅绿/暖白风格）；左侧服务状态卡片与能力目录/状态快照抽屉；中央统一时间线与折叠 `.process` 执行过程卡；右侧模拟车辆 SVG 俯视图（四窗 + 空调）、待回复倒计时、在途指令手动回执面板 |
| **W05** | [app.js](../../cmd/server/web/app.js)、[mock-web-api.py](../../scripts/mock-web-api.py) | `POST /api/session` Cookie 建连、`GET /api/config` / `capabilities` / `state` 加载、`EventSource /api/events/stream`（`Last-Event-ID` + `stream.reset` + 退避重连）、流式 `POST /voice/utterances` 解析、`(turn_id, step_id)` 双流幂等去重、100ms 实时计时器与 JSON 观测展开、手动 `POST /debug/receipt` 交互 |
| **W06** | [pcm-worklet.js](../../cmd/server/web/pcm-worklet.js)、[voice-client.js](../../cmd/server/web/voice-client.js) | `AudioWorklet` 实时重采样 16 kHz 单声道 PCM16LE；严格对接 `GET /voice/audio` PTT 协议（`session.open` → `session.ready` → `speech.started` → 顺序 `audio.input` → `speech.ended` → `playback.*` 24 kHz 队列播报 → `playback.ack`）；起讲打断停播、`result-*` 本地播放去重与 `session.reconnecting` 代数更新 |

---

## 2. 自动化验证记录

### 2.1 静态与安全审计

```bash
node --check cmd/server/web/app.js
node --check cmd/server/web/voice-client.js
node --check cmd/server/web/pcm-worklet.js
python3 -m py_compile scripts/mock-web-api.py
git diff --check
```

- 全部 3 个 ES Module / Worklet 文件语法检查通过；
- 校验 `index.html` 中 77 个唯一 DOM ID 与 `app.js` 引用的全部 58 个 ID 100% 对齐，无外部 CDN 依赖；
- 校验 `app.js` / `voice-client.js` **零 `innerHTML` / `outerHTML`**（全部采用 `createElement` + `textContent`），且 URL 中不传递任何 Token。

### 2.2 无头浏览器端到端验收（Chromium + Playwright）

针对冻结契约模拟器（`python3 scripts/mock-web-api.py --sim-mode accepted_only`），使用真实 Chromium 验证：

1. **会话初始化与 SSE 实时同步**：自动完成 `POST /api/session`（签发 `HttpOnly; SameSite=Strict` Cookie）、加载 `/api/config`、`/api/capabilities`（4 项）、`/api/state`，`#sseStatus` 进入「实时同步」，右侧俯视图显示 `左后窗：开启`。
2. **场景 1（文字两轮关窗 + 双流 SSE 去重）**：
   - 第一轮输入「把车窗关一下」→ 流式呈现 `BM25 能力预筛`、`Hermes 意图理解`、`MCP create_vehicle_task` 3 步折叠卡片，返回追问「要关哪扇车窗？」，待办计数变为 `1` 且输入框上方显示待回复倒计时条；
   - 第二轮输入「左后窗」→ 续办原任务并进入 `executing`，在途指令区出现 `[模拟完成]` / `[模拟失败]` 按钮，双 SSE 通道合并后无重复步骤或重复气泡。
3. **场景 2（`SIM_MODE=accepted_only` 手动回执闭环）**：
   - 点击 `[模拟完成]` 触发 `POST /debug/receipt` → 通过 `GET /api/events/stream` 收到 `executor.receipt`、`telemetry.updated`、`vehicle.result`；
   - 右侧模拟车况面板 `左后窗` SVG 与文字实时翻转为「已关闭」，时间线追加唯一权威结果卡片「车辆回执确认：车窗已关闭。」。
4. **场景 3（能力目录与状态快照抽屉）**：打开能力目录并展开 JSON Schema 预览、打开 `/api/state` 快照面板均无控制台或页面错误（`0 pageerror`, `0 console.error`）。

### 2.3 PTT 语音协议与 PCM 单元测试（Node 协议状态机验证）

- 验证 `pcm-worklet.js` 重采样算法（48 kHz → 16 kHz 3:1 抽取与 44.1 kHz → 16 kHz 线性插值）输出精确的 16 kHz 16-bit LE PCM 帧长（每 20 ms 固定 640 字节、无越界削波）。
- 验证 `VehicleVoiceClient` 在模拟 WebSocket + 模拟 `AudioContext` 下的完整 PTT 帧序列：
  - `session.open`（16 kHz/mono/`pcm_s16le`）→ 接收 `session.ready`（记录 `connection_generation`）
  - `startTalking()` → 停播旧音频并发送 `speech.started` → 连续 `audio.input`（`sequence = 1, 2, ...`、Base64 偶数字节、30 秒上限保护）→ `stopTalking()` 刷出尾帧并发送 `speech.ended`
  - 下行 `playback.start` → 顺序 `audio.output`（24 kHz PCM16LE 解码与调度）→ `playback.stop(completed)` → 播完自动发送 `playback.ack`
  - 重复推送同一 `result-*` 播放 ID 时本地跳过重播并直接 ACK；`session.reconnecting` 触发时安全中止当前录音并等待新代数 `session.ready`。

---

## 3. 真实 Go 后端合入联调说明

- 当前 Agent A 的后端分支（W01 已合入 `main`，W02/W03 正在收尾）合入 `main` 后，可直接执行 `./scripts/start-demo.sh` 并访问 `http://127.0.0.1:8080/` 进行真实 Hermes + 百炼 Qwen Realtime 联调：
  - **文字两轮关窗**：「把车窗关一下」→「左后窗」；
  - **语音两轮关窗 + 闲聊**：点击「连接语音」后按住说话或按住空格键；
  - **两阶段回执演示**：以 `SIM_MODE=accepted_only ./scripts/start-demo.sh` 启动，在右侧模拟车况面板点击 `[模拟完成]` 观察实时 SSE 回执与车窗关闭动画。

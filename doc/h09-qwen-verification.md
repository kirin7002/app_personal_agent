# H09 百炼 Qwen Realtime

`realtime/qwen` 使用 `qwen-audio-3.1-realtime-flash` 和业务空间专属 WebSocket。Go 持有 Bearer key，成功收到 `session.updated` 后才开放音频。输入 16 kHz、输出 24 kHz，均为单声道 PCM16LE。车控轮次仅提交音频并读取最终转写。普通会话在分流后显式请求生成。

官方说明见 [WebSocket 接入](https://help.aliyun.com/zh/model-studio/audio-realtime-websocket-access)、[客户端事件](https://help.aliyun.com/zh/model-studio/fun-audiochat-client-events) 和 [服务端事件](https://help.aliyun.com/zh/model-studio/qwen-audio-realtime-server-events)。

## 验证

```sh
go test -race ./realtime/qwen -v
go test -race ./...
go vet ./...
git diff --check
```

协议模拟通过。覆盖配置确认、单连接两次提交、关联最终转写、旧响应音频隔离、PCM 流、取消终态、配置确认超时、关闭码 1002、转写失败及新建连接。模拟服务要求先完成两次提交和转写才接收 `response.create`。

## 真实音频探针

服务端环境需配置 `DASHSCOPE_API_KEY`。通过 `DASHSCOPE_WORKSPACE_ID` 使用专属域名，缺省时北京和新加坡支持区域域名。可选 `DASHSCOPE_REGION`（默认 `cn-beijing`）和 `DASHSCOPE_MODEL`。使用实际普通话录音，说明示例期待读到关窗指令和一段普通对话。

```sh
ffmpeg -i car.wav -f s16le -ac 1 -ar 16000 car.pcm
ffmpeg -i chat.wav -f s16le -ac 1 -ar 16000 chat.pcm
go run ./cmd/qwen-probe -car car.pcm -chat chat.pcm -out reply.pcm
ffplay -f s16le -ar 24000 -ac 1 reply.pcm
go run ./cmd/qwen-probe -car car.pcm -chat chat.pcm -out interrupted.pcm -cancel-after-first
```

探针报告握手、会话配置确认、两次最终转写和首帧音频。只有两个不同的输入项均已收到最终转写，才为后一项发送 `response.create`。使用 `-cancel-after-first` 检查取消终态。录音和音频输出放在工作树外。

真实探针已通过北京区域域名验证。转写、出声、取消及时延结果见 [H07 联调报告](h07-end-to-end.md#百炼独立实测)。


## 指定文本探针

```sh
go run ./cmd/qwen-probe -read-text '左后车窗已关闭。' -out read-aloud.pcm
```

探针在独立会话中写入用户文本，使用 `response.audio_transcript.done` 比对同一响应的字幕，匹配原文且收到音频时保存 PCM。北京区域域名实测字幕为「左后车窗已关闭。」。这是一次指定文本样本验证。车控确认播报使用网关注入的 TTS。[百炼客户端事件](https://help.aliyun.com/zh/model-studio/fun-audiochat-client-events) · [百炼服务端事件](https://help.aliyun.com/zh/model-studio/qwen-audio-realtime-server-events)

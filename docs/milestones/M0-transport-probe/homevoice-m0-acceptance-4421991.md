# Homevoice M0 修复候选独立验收报告（4421991）

日期：2026-10-22

本文件导入上一轮修复候选的独立复验记录；当前证据整理不构成新的独立验收。原失败候选报告保留为 [4152a4c 历史报告](homevoice-m0-acceptance-4152a4c.md)。正式离线证据与来源说明见 [evidence/reconciliation.md](evidence/reconciliation.md)。

## 判定

**修复版候选离线验收：通过。M0 总体：仍阻塞，未完成。**

本次复验固定在修复提交 `442199166be14df64d43b56d38b01fcddbffcf61`。此前报告中记录的 F1–F4 缺陷已完成修复，并由新增回归测试覆盖。完整离线验收入口通过，A01–A28 全部 PASS。

真实云端、Remote MCP、Home Assistant、硬件和公网安全验证没有执行，因此本报告不代表完整 M0 通过，也不授权公网部署、OTA、HA 写操作或真实设备写操作。

## 固定范围

| 项目 | 值 |
|---|---|
| 仓库 | `/Users/kevin/Documents/works/homevoice` |
| PR | https://github.com/kevin4044/homevoice/pull/1 |
| 分支 | `feature/m0-transport-probe` |
| 固定 HEAD | `442199166be14df64d43b56d38b01fcddbffcf61` |
| 提交 | `4421991 docs(evidence): address independent acceptance F1-F4 and refresh evidence` |
| 工作区 | clean |
| 验收依据 | 新仓库 `docs/milestones/M0-transport-probe/` 的 M0-v0.1 规格、协议与验收矩阵 |
| 验收方式 | 固定提交上的独立静态复核、回归测试检查和完整离线验收 |

实现意图仍是通过隔离探针验证可信 MQTT 身份、UDP/Opus、播放与通知生命周期、设备 MCP 及当前设备工具绑定，为后续真实兼容性验证提供底座。它不是生产服务，也不是完整 M0 完成证明。

## 复验结论摘要

| 范围 | 结果 |
|---|---|
| F1 语音轮次所有权 | 已修复，回归通过 |
| F2 取消后的工具发送 | 已修复，回归通过 |
| F3 SSE 持流响应 | 已修复，回归通过 |
| F4 音量 schema 约束 | 已修复，回归通过 |
| gofmt | PASS |
| go vet | PASS |
| race 测试 | PASS |
| A01–A28 | 全部 PASS |
| L01–L03 云端验证 | BLOCKED，未执行 |
| H01–H06 真机验证 | BLOCKED，未执行 |

## F1：语音轮次所有权

### 原问题

同一 MQTT 连接代次内，旧语音轮次退出时仅比较 connection generation，可能清除新轮次的 `pipelineBusy`，并覆盖新轮次的取消句柄。这样会导致新轮次仍在 ASR/TTS 时错误报告 Idle，并可能放行设备通知。

### 修复确认

修复版在调度前持锁建立独立的 pipeline round ownership：

- 为每轮分配递增 `pipelineToken`。
- 在启动 goroutine 前固定 `sessionID`、`pipelineCancel` 和 round token。
- `finishPipeline` 只在 token 仍为当前 token 时清理状态。
- 旧轮晚退出时不会清理新轮的 busy 状态，也不会清除新轮取消句柄。
- 回归覆盖同一 session 和重新 HELLO 两种场景。

相关代码：[probe.go](../../../internal/m0/probe/probe.go#L861-L929)

相关测试：[a20_pipeline_test.go](../../../internal/m0/acceptance/a20_pipeline_test.go)

### 结果

**PASS。** `TestA20OldPipelineMustNotClearNewBusy` 及 `same_session`、`rehello` 子测试通过；新轮次保持 busy，旧轮次退出不会错误放行 notify。

## F2：取消后的工具调用

### 原问题

旧实现会在检查 `ctx.Done()` 前调用同步 `SendMCP`。即使调用已经取消，具有副作用的 `tools/call` 仍可能被发布；返回取消错误不能撤回已发布请求。

### 修复确认

修复版：

- 在调用入口检查取消。
- 在序列化和注册 pending 请求后、发送前再次检查取消。
- 优先使用可取消的 `ContextSender`。
- 对旧同步 sender 使用有界等待，避免调用方永久阻塞。
- 发送后若调用方取消，明确返回取消状态，不自动重试副作用请求。
- 测试覆盖预取消、context-aware sender 取消、deadline 以及 legacy sender 有界退出。

相关代码：[devicemcp.go](../../../internal/m0/devicemcp/devicemcp.go#L583-L665)

相关测试：[cancel_test.go](../../../internal/m0/devicemcp/cancel_test.go)

### 结果

**PASS（本次回归范围）。** 预取消的 `tools/call` 发送计数为零；支持 context 的测试 sender 能观察取消或 deadline 并退出。旧同步 sender 的调用方等待有界，但底层发送 goroutine 无法被强制停止，仍可能继续执行，因此不能将其描述为发送本身已取消或资源已完全收口。已发布或仍在执行中的请求，其副作用结果可能不确定；未加入自动重试。

## F3：SSE 持流响应

### 原问题

旧实现先读取整个响应直到 EOF，再解析 SSE。服务器已经 flush 完整匹配事件但保持连接时，客户端会错误等待到超时。

### 修复确认

修复版：

- 使用增量 SSE 读取和事件分发。
- 消费到完整、匹配 request ID 的 JSON-RPC result/error 后立即返回。
- 不再要求服务器关闭流。
- 保留响应总字节限制和 SSE 事件数量限制。
- 保留 context 取消、deadline、半包、无匹配 ID、超限和错误响应处理。
- 普通 `application/json` 响应路径仍单独处理。

相关代码：[transport.go](../../../internal/m0/remotemcp/transport.go#L103-L145) 和 [transport.go](../../../internal/m0/remotemcp/transport.go#L250-L327)

相关测试：[sse_stream_test.go](../../../internal/m0/remotemcp/sse_stream_test.go)

### 结果

**PASS。** 完整匹配 SSE result/error 在流保持开启时即可完成；半包、无匹配、超限、取消和超时场景均保持有界。

## F4：音量工具 schema 约束

### 原问题

旧实现只检查目标参数是否 required，忽略额外 required 字段，并丢失 `enum` 约束。模型可能调用设备实际无法接受的参数，例如在 `[0, 50, 100]` 中调用 `20`。

### 修复确认

修复版：

- 额外 required 字段无法由 volume-only executor 映射时拒绝绑定。
- optional 字段仍允许存在，但执行器只发送已绑定的 volume 参数。
- 读取并保留整数 enum。
- 合成工具 schema 暴露 enum。
- executor 在本地拒绝不在 enum 中的值。
- 空 enum、非整数 enum、越界 enum 和 malformed enum 均拒绝绑定。
- 目标设备仍来自已鉴权 session，模型不能提供任意 device ID。

相关代码：[loop.go](../../../internal/m0/agent/loop.go#L422-L479) 和 [loop.go](../../../internal/m0/agent/loop.go#L482-L603)

相关测试：[schema_restrictions_test.go](../../../internal/m0/agent/schema_restrictions_test.go)

### 结果

**PASS。** 额外 required schema 不再暴露；合法 optional 字段仍可绑定；enum 被暴露并在执行前校验；非法 enum 值不会到达设备调用器。

## 离线执行证据

复验命令：

```bash
GIT_OPTIONAL_LOCKS=0 make verify-m0 \
  CACHE=/tmp/homevoice-m0-review-new/cache \
  EVIDENCE=/tmp/homevoice-m0-review-new \
  GOMODCACHE=/Users/kevin/Documents/works/homevoice/.cache/gopath/pkg/mod \
  GOPROXY=off
```

环境：

- macOS `darwin/arm64`
- Go `1.27.1`
- `CGO=1`
- libopus `1.6.1`
- 使用已有模块缓存，`GOPROXY=off`

结果：

- 命令退出码：`0`
- `gofmt`：PASS
- `go vet`：PASS
- `go test -race -count=1 ./...`：PASS
- A01–A28：全部 PASS
- 工作区快照：clean

本次临时复验报告：[offline-report.md](evidence/offline-report.md)

报告中的关键自动化证据包括：

- A20：旧 pipeline 不会清理新 pipeline 的 busy 状态。
- A24：schema required/enum 约束和 session-bound tool 通过。
- A25：取消、deadline、sender 有界退出和副作用语义通过。
- A26：SSE 在 EOF 前完成，错误、半包、无匹配和超限场景通过。

## 未完成与安全限制

| 范围 | 状态与原因 |
|---|---|
| L01–L03 | BLOCKED；没有真实供应商、端点、费用预算及操作授权 |
| H01–H06 | BLOCKED；没有目标设备、固件、隔离网络及设备操作授权 |
| live/hardware 入口 | 实际 L01–L03 / H01–H06 流程尚未实现；即便提供配置也返回 BLOCKED |
| 固件兼容性 | MQTT TLS、配置写入/恢复、双向真实语音、Idle notify、真实 MCP 能力未验证 |
| Home Assistant | 未连接真实 HA，未执行真实读写工具 |
| 公网安全 | UDP AES-128-CTR 无认证标签；首包 peer 抢占、SSRC 可见、双向 counter 重用/重叠风险仍未解决 |
| 设备 WebSocket | 按当前范围不属于 M0；MQTT + UDP 路线保持不变 |

必须继续保持以下边界：

- UDP source locking、SSRC/template matching、replay window 和 Opus 解码都不是认证。
- MQTT publish 成功不是设备播放成功证明。
- 模拟测试通过不等于真实硬件通过。
- 离线验收通过不等于公网部署批准。
- M0 不授权 OTA、HA 写操作或真实设备写操作。

## 最终结论

**修复版 `442199166be14df64d43b56d38b01fcddbffcf61` 的离线候选验收通过。**

F1–F4 已修复，A01–A28 全部通过，工作区干净，离线验证证据已在本次复验中刷新。

**M0 总体仍为阻塞状态。** 在完成 L01–L03、H01–H06、真实固件兼容性验证以及 UDP 公网安全方案决策之前，不得将 M0 宣布为整体完成或部署到公网。

仓库正式证据已按上一轮独立复验的 `4421991` clean 快照整理；原始输出、文字勘误和本轮未提交报告工具修改的验证分别记录在 [证据核对说明](evidence/reconciliation.md)。这些整理尚未提交，不改变 M0 总体阻塞判定。

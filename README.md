# Homevoice

面向家庭的 IoT 语音控制服务：连接小智设备、云端语音与 LLM 服务，以及家庭 Home Assistant。

**当前状态：** 项目初始化，仅包含规格与协作文档；尚无可运行服务，也未完成硬件或安全验收。

## 产品目标

- 小智设备通过鉴权连接接入服务端，支持流式对话、打断和重连。
- 内部 Agent 将工具调用绑定当前设备会话，控制音量、屏幕亮度等固件支持的能力。
- 通过私有网络中的 MCP 服务协同 Home Assistant，按房间控制获准的家庭设备。
- 提供设备配对与撤销、房间管理、通知、OTA 和主题。
- 按实际需要逐步增加受控记忆、Skills 与知识检索。

## 技术方向

Go 模块化单体后端、React + TypeScript 前端、SQLite 存储。初期采用 HTTPS/WSS，家庭 HA 不直接暴露公网。具体依赖与固件兼容性在 M0 验证后确定。

不建设通用 AI 平台，不引入飞书等消息渠道、编程 Agent、多租户或海量厂商配置。

## 文档入口

- [Agent 工作规则](AGENTS.md)
- [产品路线图与里程碑](docs/roadmap.md)
- [架构与安全边界](docs/architecture.md)
- [协作与知识边界原则](docs/governance/agent-principles.md)
- [里程碑交接与验收流程](docs/governance/milestone-workflow.md)

下一阶段是 M0：验证真实设备协议、Go 音频链路、模型工具调用与 MCP 接入。本仓库初始化不构成 M0 的执行授权。

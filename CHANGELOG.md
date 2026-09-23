# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added
- **`agsw login <name>`**：独立 OAuth2 授权码流程（PKCE S256 + 127.0.0.1 本地回环回调），支持多平台默认浏览器自动拉起与凭据直写账号池。
- **`agsw serve`**：反向代理服务，支持 Gemini 与 Cloud Code 双向信封改写、SSE 流式解壳、模型别名转换与请求级凭据注入。
- **额度感知与轮换**：后台轻量轮询上游配额接口，配额耗尽自动标记账号冷却；配合代理上游 429 响应即时触发快速探测与换号。
- **`agsw probe`**：流量探针工具，支持 HTTP 网关与 CONNECT 代理透明抓包诊断，敏感凭据自动脱敏。
- **账号池管理**：`add`、`list`、`status`、`drop` 命令，凭据文件采用严苛权限控制与原子写入。

### Changed
- **选号器细粒度并发控制**：Token 网络刷新重构为单账号互斥与 double-checked locking，解除全局锁对其他健康账号的读写阻塞。
- **代理 429 响应被动联动**：新增 `StatusReporter` 接口，代理层拦截 429 立即触发账号临时避让并唤醒配额轮询协程。
- **全平台浏览器拉起适配**：抽离操作系统分发逻辑，支持 Linux (`xdg-open`)、macOS (`open`) 与 Windows (`rundll32`)。

### Fixed
- **超限/异常响应体泄漏与截断修复**：`readAllLimited` 修复边界字节截断问题，超限时利用 `io.MultiReader` 保留已读字节并无缝回退透传流，避免连接泄漏。
- **OAuth 回调服务平滑退出**：回调处理器显式刷新 HTTP 响应并在后台优雅关闭，消除高并发下本地 TCP 提前断开 (EOF) 竞争。
- **SSE 换行符跨端归一化**：规范化换行处理，防止上游混合 CRLF/LF 导致客户端事件解析停滞。

### Security
- 凭据文件采用 0600 权限与临时文件原子重命名写入，账号名称严格校验防止路径穿越。
- 诊断探针自动脱敏 `Authorization`、`Cookie` 等关键凭据。
- 额度冷却状态保持纯内存维护，避免污染持久化凭据。

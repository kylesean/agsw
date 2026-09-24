# agsw

**中文文档** | [English](README.en.md)

Antigravity CLI (`agy`) 多账号切换与反向代理管理工具。

## 背景

`agy` 默认将 OAuth 凭据保存在系统的 Secret Service（GNOME Keyring / macOS Keychain / Windows Credential Manager）中，且同一时刻仅保存单个凭据条目。当个人账号达到 Google 额度限制（5 小时配额或每周配额）时，开发者需要频繁手动重新登录并覆盖现有凭据。

`agsw` 通过独立私有账号池、协议双向改写反向代理以及智能配额感知联动，实现免频繁手动登录的多账号平滑轮换与自动切号。

## 架构与协议转换原理

`agy` 网关模式与原生模式通信协议存在差异，`agsw serve` 作为反向代理在中间负责双向协议转换：

- **客户端接口**：`agy` 网关模式使用标准 Gemini REST 协议：
  `POST /v1beta/models/{model}:streamGenerateContent?alt=sse`
- **上游服务接口**：后端接入 `daily-cloudcode-pa.googleapis.com`：
  `POST /v1internal:streamGenerateContent?alt=sse`
- **信封结构转换**：
  ```
  agy (客户端)  POST /v1beta/models/{model}:streamGenerateContent?alt=sse
                 body = 标准 Gemini GenerateContentRequest
  agsw (代理)    POST /v1internal:streamGenerateContent
                 body = {"model":"<上游真实键>","request":<原 body 字节>}
  上游服务       data: {"response":{candidates...},"traceId":...,"metadata":{}}
  agsw (代理)    data: {candidates...} （解包还原回标准 REST 结构）
  ```

### 协议与传输关键点

1. **User-Agent 鉴权闸门**：
   上游服务对请求头具有鉴权校验，代理层统一注入客户端标识 `User-Agent: antigravity-cli/1.2.9`。
2. **模型名自动映射**：
   客户端发出的模型名（如 `gemini-3.8-flash`）在上游可能存在特定后缀标识（如 `gemini-3.8-flash-tiered`）。代理启动时自动通过 `fetchAvailableModels` 获取模型表并推导别名映射。
3. **SSE 换行符归一化**：
   上游流式返回使用 CRLF，代理层通过流式解析器统一归一化为 LF 换行，确保客户端 SSE 解析器稳定分发事件。
4. **代理出网支持**：
   代理客户端默认继承系统环境变量代理（`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`）。

## 安装方式

### 方式 1：Go 工具链安装（推荐）

```sh
go install github.com/kylesean/agsw/cmd/agsw@latest
```

### 方式 2：下载预编译二进制

从 [GitHub Releases](https://github.com/kylesean/agsw/releases) 下载适用于你的系统架构（Linux / macOS / Windows）的预编译包，解压后放入系统 `PATH` 目录即可。

## 命令说明

```sh
agsw probe    # 启动拦截探针（记录线格式与协议交互）
agsw login    # 独立执行 Google OAuth 登录入池（推荐）
agsw add      # 捕获当前系统 Keyring 中的凭据入池
agsw list     # 列出账号池中的所有账号
agsw status   # 查看当前系统 Keyring 账号与账号池状态
agsw drop     # 从账号池中移除指定账号
agsw usage    # 查询账号池中各账号的真实额度
agsw          # 推荐：直接启动 Gateway 并拉起 agy
agsw gui      # 兼容别名：等价于 agsw
agsw serve    # 仅启动反向代理
```

### 账号登录 (`login`)

`agsw login <name>` 独立完成 OAuth 2.0 授权码 + PKCE 流程，不依赖系统 Keyring：

```sh
agsw login A                          # 拉起浏览器登录并存入账号池
agsw login -email you@gmail.com B     # 预填邮箱
agsw login -no-browser C              # 仅打印授权 URL（适用于 SSH/无头环境）
agsw login -timeout 10m D             # 指定超时时间（默认 5m）
```

> 注：命令行参数需写在 `<name>` 前面。

流程规范：
1. 本地监听随机端口（仅绑定 `127.0.0.1:0`），路径 `/auth/callback`。
2. 生成 `state`（防 CSRF）与 PKCE `code_verifier`（S256 挑战码）。
3. 调起系统默认浏览器打开授权链接，并在终端输出链接兜底。
4. 回调接收 `code` 并换取完整令牌（含 `refresh_token`），校验 `id_token` 身份后原子写入账号池文件。

### 代理启动 (`serve`)

```sh
# 默认配置启动代理（监听 127.0.0.1:8085）
agsw serve

# 配合 agy 网关接入
AGY_GATEWAY_URL=http://127.0.0.1:8085 agy --print 'hi'

# 常用选项
agsw serve -account main      # 仅使用指定账号
agsw serve -refresh           # 启动时强制预刷新 Token
agsw serve -v                 # 详细日志输出每次选号与转发详情
agsw serve -quota-interval 1m # 配额轮询间隔（默认 1 分钟）
```

### 统一启动 (`gui`)

`gui` 是推荐入口：它会启动 Gateway，等待本地端口 ready，自动设置
`AGY_GATEWAY_URL` 和 `NO_PROXY`，然后拉起 `agy`；`agy` 退出时自动关闭 Gateway。
Gateway、额度轮询和账号切换日志默认写入 `~/.cache/agsw/gui.log`，不会插入 agy 的
TUI 界面；可用 `tail -f ~/.cache/agsw/gui.log` 查看。

交互式 `agy` 默认启用 `-sync-keyring`：实际选中的账号变化时，`gui` 会先把新账号
凭据写入系统 Keyring，再只重启自己启动的 `agy`。重启后执行 `/resume` 恢复原会话，
GUI 顶部账号会读取新的 Keyring 身份。agy 原生 `/usage` 是否经过 Gateway 取决于其自身实现，
当前不保证由 agsw 代为代理；手动启动的 `agy` 不会被杀掉。

```sh
# 启动交互式 agy（推荐，最短用法）
agsw

# 等价写法：显式使用兼容别名
agsw gui

# 把 agy 参数放在 -- 后面；额度低于 0.2% 时提前切换
agsw -quota-threshold=0.002 -- --dangerously-skip-permissions

# 透传任意 agy 参数
agsw -- --print 'hi'

# 不修改系统 Keyring，也不自动重启
agsw -sync-keyring=false
```

一次性 `agy --print` 不启用自动重启；它只通过 Gateway 发送请求。实际后端账号以
`agsw` 的 `选号` 日志为准。agy 原生 `/usage` 是否经过 Gateway 取决于 agy 自身实现；
需要查看真实账号池额度时，推荐直接运行：

```sh
agsw usage
agsw usage -account B
```

上游返回 429 且响应尚未开始输出时，Gateway 会将当前账号短暂冷却，并用下一个账号
重放一次请求。


### 额度检测与自动切号

1. **只读端点感知**：
   后台定期轮询 `v1internal:retrieveUserQuotaSummary`，仅查询配额窗口，不消耗生成额度。
2. **多窗口计算**：
   解析 GEMINI 模型的 5 小时滑动窗口与周窗口，剩余比例低于阈值（默认 `<= 0`）即判定耗尽，自动标记账号冷却至 `resetTime`。
3. **即时 429 联动**：
   当上游返回 429（Too Many Requests）时，代理立即将当前账号标记短期临时冷却（1 分钟），并异步唤醒配额检测，平滑切换至下一个可用账号。
4. **细粒度并发续期**：
   选择器采用单账号粒度的锁同步控制，单个账号的 Token 刷新不会阻塞其他健康账号的选择与使用，同时防止多请求并发触发刷新风暴。

### 捕获已有凭据 (`add`)

若已通过 `agy` 正常登录过账号，可直接捕获进池：

```sh
agsw add <name>
```

## 安全与权限控制

- **本地存储安全**：账号池目录强制权限 `0700`，凭据文件强制权限 `0600`，写入采用临时文件原子替换。
- **输入合法性校验**：账号名称强制校验白名单正则 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`，严格禁止路径穿越。
- **敏感信息脱敏**：探针与调试日志中对 `Authorization`、`Cookie` 等鉴权头执行脱敏掩码，避免 Token 泄露。
- **Keyring 写入受控**：`add` 和 `status` 仍只读；只有 `agsw gui` 管理交互式 `agy` 时，账号切换才会同步 Keyring 并重启它。可用 `-sync-keyring=false` 完全关闭写入。

## 风险声明

多账号轮换用于在合规前提下管理开发与测试凭据，请严格遵守相关平台的服务条款。

## 开发与测试

```sh
go build ./...
go test -v -race ./...
go run ./cmd/agsw -h
```

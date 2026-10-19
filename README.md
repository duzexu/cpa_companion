# CPA Linux DO Companion

独立运行的 CPA 附属服务。用户通过 Linux DO OAuth2 登录，管理员按上游账号和时间窗创建分流方案并分配给用户；用户在“我的密钥”创建、复制或轮换 API Key。仪表盘展示请求趋势、Token 用量、自己的方案详情和所有用户可见的方案分配列表；使用日志按用户、模型、状态和时间查询，管理员可以查看全部用户。请求先经过附属服务校验，再使用指定账号的模型前缀转发到现有 CPA。附属服务不下载 OAuth 凭据，也不修改 CPA 的部署。

## 快速开始

1. 在原 CPA 为要分配的认证文件或 AI 提供商账号设置唯一前缀，确认 `/v1/models` 包含 `前缀/模型名`。准备 CPA 代理 API Key 和管理密钥，两者用途不同。
2. 在 Linux DO 创建 OAuth2 应用，将回调设为 `https://你的域名/auth/callback`。
3. 复制 `.env.example` 为 `.env`，填写 `PUBLIC_URL`、OAuth 凭证、`CPA_BASE_URL`、`CPA_API_KEY`、`CPA_MANAGEMENT_KEY`。`CPA_BASE_URL` 是 CPA 的单一根地址，须从附属容器可达。
4. 执行 `chmod 600 .env && docker compose up -d --build`，再用 Caddy 等反向代理将 HTTPS 域名转到宿主机 `127.0.0.1:8080`。
5. 首次登录后，在 `.env` 的 `ADMIN_SUBJECTS` 填入管理员 Linux DO ID，执行 `docker compose up -d`。管理员进入“分流方案”创建方案、配置价格并分配用户。客户端使用 `https://你的域名/v1` 作为 Base URL。

正式环境只有**独立 Docker Compose 部署**。原 CPA 可以在同机或另一台机器。如果同机 CPA 只监听 `127.0.0.1`，参见 [部署文档](docs/deployment.md) 的 host 网络覆写配置。

## 当前行为

- 方案必须选择至少一个上游账号。每个账号独立设置北京时间开始和结束时间；方案有效期是这些时段的合集。相同账号在同一时间只能属于一个方案；一个方案只能分配给一个用户。没有有效方案的密钥不能请求。
- 方案可设置每分钟请求数（RPM），默认 `0` 表示不限制。达到最近 60 秒的请求上限时返回 HTTP 429，并给出 `Retry-After`。
- 指定多个账号时按 CPA 优先级尝试，同优先级轮询；有会话 ID 的请求保持账号亲和，子代理继承父会话的账号。仅在额度不足或限流时尝试下一个账号。
- 额度预览由 CPA 管理接口实时查询，Codex 账号展示 5 小时限额和周限额及各自恢复时间。配置了手动重置次数时，页面按方案剩余次数列出上游当前最近可用的重置额度过期时间；每次展示都实时读取，不会在方案中固定某笔额度或其过期时间。用户可在方案授予的次数内手动重置，重置前有二次确认；可用次数按上游账号在方案间共享，创建方案时会扣除其他方案预留。
- 方案可选 USD 累计使用额度。“模型价格”页面按对外模型配置 New API 风格计费表达式；管理员可编辑或从 [BaseLLM](https://basellm.github.io/llm-metadata/api/newapi/ratio_config-v1-base.json) 同步，普通用户只能查看。有限额方案遇到未定价或不能计量的请求会拒绝。
- 有限额方案在请求前根据请求大小、输出 Token 上限和模型价格预扣预计费用；并发请求分别占用剩余额度，不因某条长流而排队。成功后优先按上游实际用量结算，失败时退回预扣。文本请求未指定输出上限时按 256 个输出 Token 预估。`/v1/images/generations` 按图片尺寸、质量和数量预扣，再从 CPA 图片响应的 Token 用量结算；上游缺少可用图片用量时按预扣额计费并暂停方案。`/v1/alpha/search` 预扣至少 4096 个输出 Token，响应没有用量时按请求与响应字节数估算，并在使用日志标记为估算。实际费用仍可能超过预估。
- 密钥同时保存哈希和加密副本，可从列表复制；轮换使原密钥立即失效。启动时会检查每把密钥都有加密副本。
- 用户资料、方案分配、重置计数和会话版本存入 `data/users.sqlite`；API 请求元数据、内容审计记录及请求当时的用户名快照和用户 ID 存入 `data/usage.sqlite`。仪表盘和使用日志只统计启用后的请求；常规使用日志不保存请求正文、响应正文或 API Key 明文。
- 管理员可在“分流方案”下方的“内容审计”配置 1–5 个按顺序尝试的 TypeSafe 接口、随机抽查比例、13 类风险阈值和回退关键词。接口失败、超时、限流或返回无效结果时尝试下一个；全部失败才使用关键词回退。可识别会话的首条用户输入必检。审计从本次请求整理按顺序排列的用户与助手可见文本（含历史对话和当前用户消息），过滤工具调用、工具结果、思考块、系统消息及图片；SystemOne 和关键词回退使用相同上下文。命中阈值或关键词时才在 SQLite 中保存这份文本，流量仍继续转发。CPA/OpenAI 返回结构化 `cyber_policy` 标记时也会记录同一份上下文。每条新审计记录可跳转到同一请求的使用日志。审计使用 [Vercel AI Gateway 的 TypeSafe API](https://vercel.com/docs/ai-gateway/sdks-and-apis/typesafe)：默认完整地址为 `https://ai-gateway.vercel.sh/typesafe/v1/systemone`，默认模型为 `jev-latest`；每个接口分别配置名称、完整 POST 地址、模型和 API Key，程序不会拼接路径。`/v1/decisions` 地址以对象形式传入对话上下文。
- 登录要求 Linux DO `trust_level` 至少达到 `LINUXDO_MIN_TRUST_LEVEL`（默认 2）；重复登录且资料未变化时不写用户数据库。计费增量先同步写入 `usage.journal`，再更新内存中的方案额度；日志达到 8 MiB 后合并进 `store.json`。这些文件需要一起备份。

详见 [系统逻辑](docs/architecture.md)、[部署与数据](docs/deployment.md)、[开发与验证](docs/development.md)。

本地模拟环境：`docker compose -f compose.local.yml up -d --build`，访问 <http://localhost:18080>。它使用 `test/local/data/` 持久化数据；在运行冒烟脚本前，请先阅读 [测试说明](docs/development.md)，避免改动已有测试数据。

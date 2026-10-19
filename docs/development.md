# 开发与验证

## 目录

- `cmd/companion/`：生产程序入口。
- `internal/companion/`：OAuth、管理页面、方案与价格存储、SQLite 日志、CPA 管理查询、代理转发和测试；`assets/app.css` 为所有页面共用的样式源文件。
- `deploy/`：生产与本地 mock 的 Dockerfile。
- `docs/images/`：界面预览图。
- `test/localmock/`：模拟 Linux DO 与 CPA 服务；`test/local/smoke.py`：端到端冒烟脚本。
- 根目录保留 Compose、Go module、环境示例和项目入口文档。

## 本地环境

```bash
go test ./...
go vet ./...
docker compose --env-file .env.example config --quiet
docker compose -f compose.local.yml config --quiet
docker compose -f compose.local.yml up -d --build
curl -fsS http://127.0.0.1:18080/healthz
```

访问 <http://localhost:18080>，模拟用户为 `local-admin`。`compose.local.yml` 使用 `test/local/data/` 保留现有测试数据。`test/local/smoke.py` 会创建和删除方案、分配用户、设置价格、创建并撤销密钥；**不要对已有人工测试数据直接运行**。请先复制一份 Compose 文件，改用临时数据目录、不同主机端口及匹配的 `PUBLIC_URL` / OAuth 授权地址，再对隔离实例设置 `SMOKE_BASE_URL` 运行脚本。停止服务用 `docker compose -f compose.local.yml down`，无需删除持久化目录。

## 接口范围

代理支持带顶层 `model` 字段的 JSON POST 路径：`/v1/chat/completions`、`/v1/completions`、`/v1/responses`、`/v1/responses/compact`、`/v1/messages`、`/v1/images/generations`、`/v1/alpha/search`。`GET /v1/models` 返回当前方案所选账号可用模型。所有对外 POST 路径均支持 USD 额度计费；Codex 搜索响应没有用量时按请求与响应字节数估算。`/v1/messages/count_tokens`、`/v1/audio/speech`、`/v1/tts` 以及其他未列出的路径均不对外开放。

修改转发、方案或计费时运行完整 Go 测试；修改页面时在本地模拟环境核对 `/dashboard`、`/keys`、`/admin`、`/audit`、`/prices`、`/logs` 及对话框、时间轴和 toast。根路径 `/` 会跳转到 `/keys`。生产环境请通过独立域名验证 OAuth 回调和 HTTPS 反向代理。

页面样式统一由 `/assets/app.css` 提供。新增交互脚本应通过 `window.__companionRegister(name, signal => { ...; return cleanup })` 注册，监听器使用传入的 `signal`，计时器和挂在 `document.body` 的节点由 `cleanup` 清理。侧栏切换先卸载旧模块再初始化新模块；改动交互后请至少测试“进入页面 → 操作 → 切走 → 再进入并操作”，并检查浏览器控制台。

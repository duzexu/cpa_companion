# 部署与数据

## 独立 Docker Compose

附属服务单独部署，不需要原 CPA 的源码、认证文件或第二个 CPA 实例。复制根目录 `.env.example` 为 `.env`，填写所有必填项并设置 `chmod 600 .env`。`PUBLIC_URL` 是用户访问的 HTTPS 根地址，Linux DO 回调必须精确设置为 `PUBLIC_URL/auth/callback`。`CPA_BASE_URL` 是从容器可达的 CPA 根地址；支持 HTTPS 公网地址或私网/回环 HTTP。公网 HTTP 会被拒绝。代理密钥 `CPA_API_KEY` 和管理密钥 `CPA_MANAGEMENT_KEY` 不能混用；后者需要填写 CPA 管理密钥**明文**，不是配置文件里的 bcrypt 哈希。

```bash
docker compose up -d --build
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
```

默认 Compose 仅向宿主机回环地址发布 8080。用已有 Caddy 的独立域名反向代理，例如：

```caddyfile
companion.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

如果 CPA 也在同一 Linux 主机，且管理接口仅监听宿主机 `127.0.0.1`，设置 `CPA_BASE_URL=http://127.0.0.1:8317`，用同一个 Dockerfile 的 host 网络覆写启动：

```bash
docker compose -f docker-compose.yml -f compose.server.yml up -d --build
```

同机使用此覆写且希望直接运行 `docker compose up -d --build` 时，可在服务器项目目录执行 `cp compose.server.yml docker-compose.override.yml`；Compose 会自动加载这个文件。另一种方式是在 `.env` 设置 `COMPOSE_FILE=docker-compose.yml:compose.server.yml`。单独存在的 `compose.server.yml` 不会自动加载，缺少两种设置之一会回到桥接网络，使容器内的 `127.0.0.1` 无法访问宿主机 CPA。执行前可用 `docker compose config --format json` 检查 `services.companion.network_mode` 为 `host`。此配置使附属服务监听宿主机 `127.0.0.1:8080`。请确认 CPA 实际端口及 Caddy 端口，避免与已有服务冲突。普通桥接网络部署可用于同机或异机 CPA，只要容器能访问 `CPA_BASE_URL`。

首次登录后读取页面上的 Linux DO ID，填入 `.env` 的 `ADMIN_SUBJECTS`（多个 ID 以逗号分隔），执行 `docker compose up -d` 更新环境。管理员建立方案并分配用户；用户创建密钥后，客户端 Base URL 使用 `PUBLIC_URL/v1`。

可选的 `GITHUB_URL` 控制侧边栏左下角的 GitHub 入口。填写完整 HTTPS 地址后重建附属容器；留空时不显示入口。

`LINUXDO_MIN_TRUST_LEVEL` 默认是 `2`，可设置为 `1`–`4`。Linux DO 用户信息缺少有效 `trust_level` 或低于门槛时拒绝登录，不会写入用户存储。部署前确认管理员账号符合要求，必要时在 `.env` 中调整。

## 持久化、升级与回退

`./data` 挂载到容器 `/data`。其中 `users.sqlite` 保存用户资料、方案分配、重置计数和会话版本；`store.json` 保存方案、价格、密钥加密副本、按顺序配置的审计接口及计费检查点；`usage.journal`（轮换后为 `usage.<编号>.journal`）保存检查点后的累计费用；`usage.sqlite` 保存请求使用日志（含请求当时的用户名快照）、内容审计记录及其使用日志关联；`cookie.key` 同时用于会话和各审计接口密钥加密。启用审计后，命中的对话上下文会保存在 `usage.sqlite`，备份也包含这些敏感内容。整个目录必须一起备份；只备份 `store.json` 会遗漏用户资料、使用日志及尚未合并的费用。丢失 `cookie.key` 后现有会话、可复制密钥及已保存的审计接口 API Key 无法恢复。备份和迁移时停止附属容器后复制整个目录，或使用一致性快照，不要把 `.env` 或 `data/` 提交到源码仓库。

当前格式要求 `data/users.sqlite` 保存用户资料，`store.json` 使用模型价格表达式、逐账号时间窗和 `audit.endpoints`；每把 API Key 必须有加密副本。方案整体起止时间由账号时段在内存中推导，不再写入 `store.json`。升级时先停止附属容器并完整备份 `data/`，再执行 `docker compose up -d --build`；原 CPA 不需要重启。使用日志和内容审计分别保存请求发生时的用户名快照与用户 ID，后续改名不改写历史记录。方案配置使用单机 JSON，用户与使用日志分别使用单机 SQLite；服务只能运行一个实例。回退程序时应同时恢复升级前整个 `data/` 备份。

审计接口使用完整 POST 地址，程序不会自动补路径。升级已有安装时，先停止附属容器并备份整个 `data/`；然后在 `data/store.json` 中把 `audit.endpoints` 内旧 TypeSafe 根地址的 `base_url`（例如 `https://ai-gateway.vercel.sh/typesafe/v1`）改为完整的 `https://ai-gateway.vercel.sh/typesafe/v1/systemone`，再启动新版。`/v1/decisions` 地址直接保留。旧版管理页不接受带 `/systemone` 的地址，因此不要在旧版管理页提前修改。

`CPA_MODEL_ALIAS_PREFIXES` 是可选补充，仅用于 CPA 的 OAuth 模型别名接口未列出的自定义前缀。填写后，匹配此前缀的模型不会出现在对外 `/v1/models` 或价格列表，客户端也不能直接请求。上游认证文件或 AI 提供商配置必须有可识别且唯一的前缀，才能被方案指定。

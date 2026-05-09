# deployd

一个朴素的 webhook 部署器：CI 推完镜像后调一个 HTTP 接口，deployd 在服务器上执行 `docker compose pull && docker compose up -d --wait`。

写它是为了避免 GitHub Actions 直接 SSH 到服务器（云厂商会因此告警）。

## 设计取舍

- **单职责**：只做 `docker compose pull` + `up -d --wait`。不替换 yaml、不做 tag 替换、不渲染模板。需要更新版本时让 CI 推 `:latest`。
- **HMAC 鉴权**：每个 service 一把独立 secret，签名内容 = `<timestamp>\n<body>`，时间戳 ±5 分钟窗口，内存防重放。
- **TLS 不自己做**：绑 `0.0.0.0:8080`，前面 Traefik 反代上 HTTPS。
- **零外部依赖**：除 `gopkg.in/yaml.v3` 外都用标准库。
- **状态可查**：每次部署有 `deploy_id`，最近一次状态可通过 `/deploy/{service}/status` 查询，全部历史落 `state_dir/history.jsonl`。

## 架构

```
GitHub Actions ──HTTPS+HMAC──> Traefik ──> deployd 容器 ──exec──> docker compose pull/up
                                                  │
                                                  └── /var/run/docker.sock (host)
                                                  └── /root/<service> (host bind, ro)
```

## 部署 deployd

1. 准备目录：

   ```bash
   mkdir -p /opt/deployd && cd /opt/deployd
   curl -O <repo>/deploy/docker-compose.example.yaml
   curl -O <repo>/deploy/config.example.yaml
   curl -O <repo>/deploy/secrets.example.env
   mv docker-compose.example.yaml docker-compose.yaml
   mv config.example.yaml config.yaml
   mv secrets.example.env secrets.env
   chmod 600 secrets.env
   ```

2. 为每个 service 生成 HMAC 密钥并写进 `secrets.env`：

   ```bash
   openssl rand -hex 32
   ```

3. 编辑 `config.yaml`，按需添加 services（详见示例文件注释）。

4. 编辑 `docker-compose.yaml`：
   - 改 `traefik.http.routers.deployd.rule` 里的域名
   - 把每个目标服务的 workdir 都加到 `volumes`，**容器内路径必须与宿主机一致**，可以用 `:ro`
   - 网络名 / certresolver 名按你的 Traefik 配置改

5. 启动：

   ```bash
   docker compose up -d
   docker compose logs -f deployd
   ```

   验证：`curl https://deploy.example.com/healthz` 应返回 `ok`。

## 配置参考

`config.yaml`：

```yaml
listen: 0.0.0.0:8080         # 默认 127.0.0.1:8080；容器里要绑 0.0.0.0
state_dir: /var/lib/deployd  # 默认值，建议挂 named volume

services:
  gpt-image-2-web:
    secret_env: GPT_IMAGE_2_WEB_SECRET   # 必填，对应环境变量必须有值
    workdir: /root/gpt-image-2-web       # 必填，绝对路径
    compose_service: gpt-image-2-web     # 可选；不填则整个 compose pull/up
    timeout: 5m                          # 默认 5m，最大 30m
```

service 名字必须匹配 `^[a-zA-Z0-9._-]{1,64}$`（用作 URL 路径）。

## HTTP API

### `POST /deploy/{service}`

触发部署。

请求头：
- `X-Timestamp`：unix 秒
- `X-Signature`：`sha256=<hex>`，HMAC-SHA256 over `<timestamp>\n<body>`
- `Content-Type: application/json`

请求体（可选）：

```json
{ "tag": "master-abc1234" }
```

`tag` 仅用于审计记录，不参与命令构造。如果传则必须匹配 `^[a-zA-Z0-9._-]{1,128}$`。

响应：

| 状态码 | 含义 |
|---|---|
| 202 | 已接受，部署在后台运行。返回 `{"deploy_id":"...","state":"running"}` |
| 400 | body 不合法 / tag 格式不对 |
| 401 | 验签失败 / 时间戳过期 / 重放 |
| 404 | service 未配置 |
| 409 | 该 service 上一次部署还在跑 |
| 500 | 内部错误（看 journald / docker logs） |

### `GET /deploy/{service}/status`

查询最近一次部署状态。**也需要验签**（body 为空，签名内容仍是 `<timestamp>\n`）。

响应示例：

```json
{
  "deploy_id": "1715251200-a1b2c3d4e5f6",
  "service": "gpt-image-2-web",
  "state": "success",
  "tag": "master-abc1234",
  "started_at": "2026-05-09T06:00:00Z",
  "finished_at": "2026-05-09T06:00:42Z",
  "duration_ms": 42000,
  "tail": "..."
}
```

`state` 枚举：`idle` / `running` / `success` / `failed`。

### `GET /healthz`

不需要鉴权。返回 200 + `ok\n`。

## GitHub Actions 调用示例

替换原来 SSH `deploy` job：

```yaml
deploy:
  needs: [ci, publish-image]
  if: needs.publish-image.result == 'success'
  runs-on: ubuntu-latest
  steps:
    - name: Trigger deployd
      env:
        URL: ${{ secrets.DEPLOY_WEBHOOK_URL }}      # https://deploy.example.com/deploy/gpt-image-2-web
        SECRET: ${{ secrets.DEPLOY_WEBHOOK_SECRET }} # 与 server 端 GPT_IMAGE_2_WEB_SECRET 一致
        TAG: ${{ needs.ci.outputs.image_tag }}
      run: |
        set -euo pipefail
        ts=$(date +%s)
        body=$(printf '{"tag":"%s"}' "$TAG")
        sig_hex=$(printf '%s\n%s' "$ts" "$body" \
          | openssl dgst -sha256 -hmac "$SECRET" -hex \
          | awk '{print $NF}')
        sig="sha256=$sig_hex"

        http_code=$(curl -sS -o /tmp/resp.json -w '%{http_code}' \
          -X POST "$URL" \
          -H "X-Timestamp: $ts" \
          -H "X-Signature: $sig" \
          -H "Content-Type: application/json" \
          -d "$body")
        cat /tmp/resp.json; echo
        if [[ "$http_code" != "202" ]]; then
          echo "deploy trigger failed with HTTP $http_code" >&2
          exit 1
        fi
```

需要的 GitHub secrets：`DEPLOY_WEBHOOK_URL`、`DEPLOY_WEBHOOK_SECRET`。可以删掉原来的 `DEPLOY_SSH_PRIVATE_KEY` / `DEPLOY_KNOWN_HOSTS` / `DEPLOY_HOST` / `DEPLOY_USER`。

## 关键运维注意点

### bind mount 必须用相同路径

deployd 容器执行 `docker compose pull` 时，compose 客户端会读 yaml，把里面相对 volume 路径（`./data:/data`）展开成宿主机绝对路径再传给 daemon。如果容器里 workdir 路径和宿主机不一致，daemon 找不到对应路径 → 启动失败。

```yaml
# 对：
- /root/myapp:/root/myapp:ro
# 错：
- /root/myapp:/work/myapp:ro
```

### 回滚

deployd 故意不内置回滚。如果某次 `:latest` 推坏了：

```bash
ssh server
cd /root/myapp
sed -i 's#:latest#:master-abc1234#' docker-compose.yaml
docker compose up -d
```

事后再修复 CI、推新的 `:latest`，然后把 yaml 改回 `:latest`。

### 日志

- 实时：`docker logs -f deployd`
- 部署历史：deployd 容器内 `/var/lib/deployd/history.jsonl`（在宿主机就是 named volume 那个路径）

## 本地开发

```bash
# 编译
go build -o ./out/deployd ./cmd/deployd

# 跑（需要先准备 config.yaml + 设置环境变量）
GPT_IMAGE_2_WEB_SECRET=$(openssl rand -hex 32) \
  ./out/deployd -config ./deploy/config.example.yaml

# 单元测试（待写）
go test ./...
```

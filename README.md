# hubp

一个使用 Go 编写的、只读的 Docker/OCI Registry V2 拉取代理。它把多个上游 Registry 映射到同一个域名，并在本地磁盘缓存 manifest 与 blob。

## 路径映射

| Docker 命令                                                         | 上游                                 |
| ------------------------------------------------------------------- | ------------------------------------ |
| `docker pull your-domain.com/library/alpine`                        | Docker Hub `library/alpine`          |
| `docker pull your-domain.com/ghcr.io/oopsunix/hubp:latest`          | GHCR `oopsunix/hubp:latest`          |
| `docker pull your-domain.com/quay.io/coreos/etcd:latest`            | Quay `coreos/etcd:latest`            |
| `docker pull your-domain.com/gcr.io/google-containers/pause:latest` | GCR `google-containers/pause:latest` |

## 特性

- 支持 Docker Registry HTTP API V2 的拉取流程。
- 自动重写上游 `WWW-Authenticate` Bearer challenge，token 也通过本服务获取。
- 每个上游可独立配置 HTTPS 代理；Docker Hub 的 Registry 请求、token 请求和重定向下载都会使用该代理。
- 磁盘缓存 blob 和 manifest。
- blob 与 digest manifest 作为不可变对象永久缓存；tag manifest 按 TTL 刷新。
- 相同缓存键的并发未命中请求只回源一次。
- 支持 `GET`、`HEAD` 和缓存命中后的 Range 请求。
- pull-only：拒绝 push、delete 和 blob upload。
- JSON 配置，支持 `${ENV_NAME}` 环境变量替换。
- 仅使用 Go 标准库解析配置，未知字段会导致启动失败。

## 快速启动

```bash
cp config.example.json config.json
cat > .env <<'ENV'
DOMAIN=your-domain.com
DOCKER_HUB_HTTPS_PROXY=http://host.docker.internal:7890
# 可选：匿名拉取不需要填写
DOCKER_HUB_USERNAME=
DOCKER_HUB_TOKEN=
GHCR_USERNAME=
GHCR_TOKEN=
ENV

docker compose up -d --build
```

Caddy 会为 `DOMAIN` 自动申请 HTTPS 证书。域名的 A/AAAA 记录必须指向该服务器，并开放 80、443 端口。

健康检查：

```bash
curl https://your-domain.com/-/healthz
curl -I https://your-domain.com/v2/
```

拉取测试：

```bash
docker pull your-domain.com/library/alpine:latest
docker pull your-domain.com/ghcr.io/oopsunix/hubp:latest
docker pull your-domain.com/quay.io/coreos/etcd:latest
docker pull your-domain.com/gcr.io/google-containers/pause:latest
```

## JSON 配置

配置文件默认为 `config.json`。duration 字段使用 Go duration 字符串，例如 `10s`、`5m`、`2h30m`。配置加载时会先展开字符串中的 `${ENV_NAME}`，再进行严格 JSON 解码。

完整示例见 [`config.example.json`](./config.example.json)：

```json
{
  "server": {
    "listen": ":8080",
    "public_url": "https://your-domain.com",
    "read_header_timeout": "10s",
    "idle_timeout": "120s",
    "shutdown_timeout": "15s"
  },
  "cache": {
    "enabled": true,
    "dir": "/var/lib/hubp/cache",
    "manifest_ttl": "5m"
  },
  "registries": [
    {
      "name": "dockerhub",
      "path_prefix": "",
      "upstream": "https://registry-1.docker.io",
      "allowed_auth_hosts": ["auth.docker.io"],
      "https_proxy": "${DOCKER_HUB_HTTPS_PROXY}"
    }
  ]
}
```

未填写的字段会使用以下默认值：

- `server.listen`: `:8080`
- `server.read_header_timeout`: `10s`
- `server.idle_timeout`: `120s`
- `server.shutdown_timeout`: `15s`
- `cache.enabled`: `false`

JSON 不支持注释。配置中出现未知字段、非法 duration、多个顶层 JSON 值或不合法的 Registry 配置时，服务会拒绝启动。

## Docker Hub HTTPS 代理

在 `config.json` 的 Docker Hub Registry 配置中设置：

```json
{
  "name": "dockerhub",
  "path_prefix": "",
  "upstream": "https://registry-1.docker.io",
  "allowed_auth_hosts": ["auth.docker.io"],
  "https_proxy": "${DOCKER_HUB_HTTPS_PROXY}"
}
```

支持常见形式：

```dotenv
DOCKER_HUB_HTTPS_PROXY=http://127.0.0.1:7890
DOCKER_HUB_HTTPS_PROXY=http://username:password@proxy.example.com:3128
DOCKER_HUB_HTTPS_PROXY=socks5://127.0.0.1:1080
```

若代理运行在宿主机而服务运行在 Docker 中，Linux 上通常应把宿主机网关显式加入 Compose，或把代理监听到容器可访问的地址；macOS/Windows Docker Desktop 可使用 `host.docker.internal`。

## 缓存行为

缓存目录结构是实现细节，勿直接修改。响应头可用于观察状态：

- `X-Hubp-Cache: HIT`：磁盘命中。
- `X-Hubp-Cache: MISS`：回源并成功写入缓存。
- `X-Hubp-Cache: BYPASS`：HEAD、Range 未命中、鉴权错误或非缓存 API。
- `X-Hubp-Cache: MISS-NOT-STORED`：回源成功，但缓存文件无法创建。

当前版本不主动淘汰 blob。生产环境应监控缓存目录磁盘空间，并通过独立任务按需清理；清理整个缓存目录是安全的，后续拉取会重新回源。

## 私有镜像

可在对应 Registry 下配置静态用户名和 token/PAT：

```json
{
  "username": "${GHCR_USERNAME}",
  "password": "${GHCR_TOKEN}"
}
```

服务会用该凭据向上游 token 服务换取 Bearer token，Docker 客户端再通过本代理使用该 token。因为缓存由所有代理客户端共享，包含私有镜像时必须把本服务部署在可信内网，或在前置网关增加访问控制。

`forward_client_authorization: true` 可把客户端发给本代理 token 端点的 Authorization 头转发到上游，但默认关闭，避免把代理域名凭据错误发送给上游。

## 安全注意事项

- 务必配置 `server.public_url`，避免反向代理场景下根据不可信 Host 推断 token 地址。
- `allowed_auth_hosts` 是 SSRF 防护边界；只加入对应 Registry 官方 token 主机。
- `tls_insecure_skip_verify` 仅供测试，不应在生产环境开启。
- 本服务不验证客户端身份。公开部署前应在 Caddy、Nginx、Cloudflare Access 或 VPN 层增加访问控制。
- Docker 默认要求 Registry 使用可信 HTTPS。仅测试时才配置 insecure registry。

## 本地构建与测试

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/hubp
./hubp -config ./config.json
```

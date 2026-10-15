# hubp

Go 编写的开发者资源反向代理 MVP：GitHub、OCI/Docker Registry、Hugging Face。

单进程、单 HTTP 监听器、静态 JSON 配置。三个协议实现位于 `internal/adapters/`。没有 `policy`、`admin`、`observability` 模块。

## 当前支持

| 来源         | 能力                                                                                                                        |
| ------------ | --------------------------------------------------------------------------------------------------------------------------- |
| GitHub       | Raw、Release 附件、Archive，HTTPS Smart HTTP v0/v2 clone/fetch                                                              |
| Registry     | Docker Hub、GHCR、GCR、Quay、registry.k8s.io；公共镜像拉取，index/manifest/blob、GET/HEAD、Range、匿名 token 自动交换与刷新 |
| Hugging Face | 公共模型/数据集的仓库 API、tree、resolve、HEAD、HTTP 文件下载和 Range；分页链接留在代理域名                                 |
| 缓存         | 可选 SQLite + 磁盘完整对象缓存；长度/digest 校验、临时文件原子提交、条件验证、完整对象单范围、容量淘汰、重启恢复            |

只读公共资源。原生 Xet、Git LFS 对象、submodule 自动改写、私有/gated 资源、分块缓存、多实例共享目录暂不支持。冷缓存 `206` 和多范围响应直通，不作为完整对象缓存。

## 快速运行

源码要求 Go 1.26 或以上；附带的 `bin/hubp` 为 Linux/amd64 可执行文件。

```bash
go build -trimpath -buildvcs=false -o bin/hubp ./cmd/hubp
./bin/hubp
```

默认监听 `127.0.0.1:8080`，缓存关闭，可直接测试：

```bash
curl http://localhost:8080/https://raw.githubusercontent.com/octocat/Hello-World/master/README
git clone http://localhost:8080/https://github.com/octocat/Hello-World.git
```

HF 使用独立 Host。本机测试可以使用下面配置示例中的两条 `routes`：GitHub 对应 `localhost`，HF 对应 `127.0.0.1`。然后启动：

```bash
./bin/hubp -config hubp.json
HF_ENDPOINT=http://127.0.0.1:8080 \
HF_HUB_DISABLE_XET=1 \
HF_HUB_DISABLE_IMPLICIT_TOKEN=1 \
hf download hf-internal-testing/tiny-random-bert config.json pytorch_model.bin
```

HF 模式必须禁用 Xet。已经登录过 HF 的客户端还应禁用隐式 token；MVP 不向上游转发个人凭据。初始客户端验证版本为 `huggingface_hub==2.2.0`、`hf-xet==1.7.0`。协议级 Range 支持不等于保证每个版本的 CLI 在进程退出后保留临时文件。

## 配置

以 `hubp.example.json` 为生产域名配置示例。JSON 仅包含需要覆盖的字段也可以；`routes` 一旦提供就完整替换默认路由，`[]` 可关闭所有入口。未知配置字段会拒绝启动。

旧的 `github_hosts`、`hf_hosts`、`registries` 配置字段已移除。迁移时将每个域名转换为一条 route，并填写对应的 kind 和 upstream。

```json
{
  "listen": "127.0.0.1:8080",
  "public_scheme": "http",
  "routes": [
    {"domain": "localhost", "kind": "github", "upstream": "https://github.com"},
    {"domain": "127.0.0.1", "kind": "huggingface", "upstream": "https://huggingface.co"}
  ],
  "cache": {"enabled": false}
}
```

`domain` 为不带端口的小写域名或本机 IPv4 地址；同一域名只能绑定一条路由。`kind` 为 `github`、`registry` 或 `huggingface`。`upstream` 为该适配器支持的 HTTPS origin，不带路径前缀、凭据或查询参数。GitHub 保留完整 URL 前缀用法，Raw/codeload 由适配器处理。相同 kind/upstream 的多个域名复用同一个适配器实例。

监听器按 Host 分发，配置中不带端口。GitHub 前缀 URL 不经过通用 ServeMux 的路径清理，保留 `https://`、编码路径和原始查询参数。

| 字段                     | 默认/含义                                                                   |
| ------------------------ | --------------------------------------------------------------------------- |
| `listen`                 | `127.0.0.1:8080`                                                            |
| `public_scheme`          | `http`；外层 TLS 终止时明确设为 `https`，用于 HF 本地链接和 commit 固定地址 |
| `routes`                 | 统一路由列表，每项包含 `domain`、`kind`、`upstream`                         |
| `upstream_proxy`         | 默认空；显式指定可信 HTTP(S) 出口代理                                       |
| `header_timeout_seconds` | 30，上游响应头等待时间                                                      |
| `stall_timeout_seconds`  | 120，上游读取或客户端写入停滞时间                                           |
| `max_upstream_requests`  | 128，实际回源请求/响应流的资源上限；超出返回 503                            |
| `cache.enabled`          | false，不初始化 SQLite 或数据目录                                           |
| `cache.dir`              | `./data`，单实例独占                                                        |
| `cache.max_bytes`        | 10 GiB，对象与临时填充的总预算                                              |
| `cache.max_object_bytes` | 5 GiB；超过时停止本次缓存写入，下载继续                                     |
| `cache.min_free_bytes`   | 64 MiB，最小剩余空间                                                        |
| `cache.ttl_seconds`      | 300，可变资源缓存上限；上游更严格的约束优先                                 |

也可用 `HUBP_UPSTREAM_PROXY` 覆盖出口代理地址；不会自动继承通用 `HTTP_PROXY`/`HTTPS_PROXY`。例如：

```bash
HUBP_UPSTREAM_PROXY=http://127.0.0.1:7890 ./bin/hubp -config hubp.json
```

直连模式在拨号前解析并校验目标地址，连接到已校验地址。配置出口代理后，目标 IP 的访问约束由可信代理负责；hubp 仍检查初始目标、认证 realm 和每次重定向的服务域名。跨域跳转清除原 Authorization/Cookie，不提供任意 URL 中转接口。

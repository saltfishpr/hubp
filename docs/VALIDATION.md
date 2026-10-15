# MVP 验证记录

日期：2026-10-15。构建工具链 Go 1.27.2，Linux/amd64，交付二进制 CGO_ENABLED=0。SQLite 驱动 modernc.org/sqlite v1.60.1。

## 自动化检查

已通过：

- go test ./...
- go vet ./...
- CGO_ENABLED=1 go test -race ./...
- go build -trimpath -buildvcs=false -o bin/hubp ./cmd/hubp

普通测试使用可控上游，不依赖公网。竞态检查包含并发填充、读取与淘汰。缓存故障检查涵盖长度/digest 错误、写入容量不足、SQL 索引提交失败、对象引用、同内容去重、目录独占和重启恢复。SQL 提交不确定时保留已安装对象并停止新填充，重启后对账，避免删掉可能已经被索引引用的内容。

## 真实上游与客户端

以下测试均经过本次实现的 hubp，不是直接上游验证。因执行环境不允许直接 DNS/出站，服务端使用显式配置的可信 HTTP 出口代理。每个 smoke 子进程均正常退出。测试时长不作为吞吐或加速指标。

| 场景 | 结果 | 验证内容 |
|---|---|---|
| github-raw | 通过 | 13 B，记录 SHA-256 |
| github-archive | 通过 | 351 B，记录 SHA-256 |
| github-release | 通过 | 2129 B，记录 SHA-256 |
| git-v0 | 通过 | 浅克隆 + fsck，commit 7fd1a60b01f9 |
| git-v2 | 通过 | 浅克隆 + fsck，commit 7fd1a60b01f9 |
| docker.hubp.local | 通过 | amd64/arm64 manifest、配置 blob、Range、首个 amd64 layer 完整 digest |
| ghcr.hubp.local | 通过 | amd64/arm64 manifest、配置 blob、Range、首个 amd64 layer 完整 digest |
| gcr.hubp.local | 通过 | amd64/arm64 manifest、配置 blob、Range、首个 amd64 layer 完整 digest |
| quay.hubp.local | 通过 | amd64/arm64 manifest、配置 blob、Range、首个 amd64 layer 完整 digest |
| k8s.hubp.local | 通过 | amd64/arm64 manifest、配置 blob、Range、首个 amd64 layer 完整 digest |
| hf-model-cli | 通过 | config.json (548 B)；pytorch_model.bin (540217 B)；客户端外部代理不可用 |
| hf-dataset-cli | 通过 | data/train.csv (1446 B)；客户端外部代理不可用 |

HF CLI 使用 huggingface_hub 2.2.0 / hf-xet 1.7.0，禁用 Xet 与隐式 token，使用独立本地目录。客户端的 HTTP/HTTPS 代理被设置为本地不可用端口，NO_PROXY 只允许 localhost/127.0.0.1；模型和数据集仍成功下载。模型文件 SHA-256 为 9922e8996d0c7e24c7f4e7a5d9c5b7303549f4ee94de0f1138b103014b51be13。

完整的脱敏结果见 live-validation.json。网络结果依赖当时上游状态，tag 可能变化。

## 统一路由配置修订验证

配置改为 routes 数组，每项为 domain、kind、upstream。已重新通过普通测试、vet、race 和构建；新增 kind/upstream 校验、重复域名拒绝、空列表覆盖默认路由、无协议提示词域名分发、同上游适配器复用测试。旧三个字段会被拒绝，迁移说明见 README。

采用新配置重新通过 GitHub Raw、Git v2 clone/fsck、Docker Hub amd64/arm64 读取和 HF 官方 CLI 模型下载。脱敏记录见 routes-validation.json；前述 12 项记录为初版协议验收。

## 未完成的验收边界

- 没有 Docker Engine，尚未执行真实 docker pull、registry-mirrors 及其回退行为验收。
- Registry 实测覆盖所选平台的 manifest、配置及首层，不代表所有 layer 已下载、解包或启动。
- 没有测试 GB 级模型、持续负载、吞吐加速比或资源压力极限。
- 没有运行 Dockerfile 容器构建和 Caddy TLS 部署；对应文件为部署示例。
- 不包含原生 Xet、LFS、submodule 自动覆盖、私有资源。

## 二进制

Linux/amd64：bin/hubp

SHA-256：090fa4ab1f2dfe62170b50dd51da8b6c055369caea0efb0a208acf713c01442e

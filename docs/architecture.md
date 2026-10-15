# MVP 代码结构

单进程、单公共监听器，静态 JSON 配置。`cmd/hubp` 负责组装和生命周期，`server` 根据统一的 `routes` 列表按 domain 分发，按 kind 选择适配器，并把 upstream origin 注入适配器。相同 kind/upstream 的多个 domain 复用实例。三个适配器统一位于 `internal/adapters/`。

| 模块 | 边界 |
|---|---|
| adapters/github | Raw/Release/Archive、Git Smart HTTP 的路由与语义 |
| adapters/registry | 仓库、表示形式、只读 API、上游匿名 token |
| adapters/huggingface | Hub API、resolve、commit 元数据、分页与 HTTP 模式 |
| upstream | 连接池、目标域名、重定向、跨域凭据、超时和在途请求资源 |
| transfer | 公共响应头处理、流式复制、取消与完整对象交付 |
| cache | SQLite 逻辑索引、磁盘对象、填充预算、校验、淘汰与恢复 |
| config | 启动参数和配置校验 |

适配器实现 http.Handler，通过 FetchFunc 执行可能包含认证/重定向的取数。纯协议流使用 transfer.Pipe；可缓存对象使用 transfer.Object。底层不 import 适配器。不建设 policy、admin、observability 或插件注册层。

## 缓存提交

完整 200 字节流同时发给客户端和临时文件。长度/digest 验证后，文件 fsync、原子 rename、目录 fsync，再提交资源索引。传输失败不会发布完整对象；缓存写入失败可独立降级直通。完整流已经结束后的短提交有单独的有限期限，不在客户端取消后继续下载。

物理对象按内部 SHA-256 去重，资源键使用稳定身份、查询语义与表示形式，签名 URL 不作为身份。缓存读取持有对象引用，GC 不删除正在使用的对象。临时预算和对象物理大小共同计入容量。访问时间合并后在短事务批量更新。

数据库提交结果不确定时不删除刚安装的文件，防止成功索引失去对应对象；停止新填充并在重启时恢复对账。缓存关闭时不创建目录或 SQLite。

## 生命周期

停止接受新请求后，活动传输有 30 秒退出宽限期；超时取消回源并关闭连接。请求完成后停止维护协程，最后关闭缓存与连接池。基本启动和错误信息直接写标准日志，不提供指标或日志管理系统。

## 协议范围

HF 使用明确的 HTTP 兼容模式，客户端需禁用 Xet。每个 Registry 域名唯一映射到一个受支持上游，Docker Hub mirror 与其他 Registry 域名分别使用。Git clone/fetch 的 POST 被允许，写入操作、LFS、Xet 等未支持路由不通用兜底转发。

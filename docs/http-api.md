# HTTP 接口

[README](../README.md) 里那份端点表在这里，附完整的响应示例。

这是一个内部导航页：绝大多数接口是给页面自己用的，你真正会手敲的只有 `/healthz`
和 `/api/refresh`。

`/api/nav` 和 `/api/status` 会看调用方是谁：请求带上 `Authorization: Bearer <能读取配置仓的令牌>`
就能拿到被 `private: true` 标记的条目；否则那些条目、以及它们的探测结果，都不会出现在响应里。
`/api/nav` 的响应里因此多一个按请求填充的字段：

```json
"private": {"total": 3, "locked": true}
```

`total` 是被隐藏的条目数（解锁后也用它显示"还有 N 个私密链接"），`locked` 表示这次是否被隐藏；
配置里没有任何私密条目时这个字段整个不出现，前端据此决定要不要显示那把锁。
`meta.groupCount` / `meta.linkCount` / `meta.pinnedCount` 与 `meta.warnings` 都只描述**这次响应里
真正有的东西**，所以匿名请求看不出私密分组有几个，也拿不到带条目名的告警。

`/api/icon` 的索引只从**公开**条目建：只被私密条目使用的主机对任何调用者都是 404，与
「主机不在配置里」状态码和耗时都一致。`/healthz` 同理只统计公开条目，见下。
细节见 [README 的「私密链接」](../README.md#私密链接)。

`/api/nav` 里每个链接还带一个**派生**字段 `probeTarget`：它说明这个链接会不会出现在
`/api/status` 的结果里（即"服务端真的会探测它"）。前端需要它，因为"某个 URL 没有结果"本身是
有歧义的——既可能是"还没探到"，也可能是"永远不会探"，后者不该挂一个一直转的状态点。
`probeTarget` 是 `probe !== false` 且协议是 http(s) 两者同时成立。


| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/` | 页面本身（内嵌在二进制里，可被 `--web-dir` 覆盖） |
| `GET` | `/theme.css` | 由 `style:` 令牌生成的 CSS 变量 + 配置仓里的 `custom.css` |
| `GET` | `/favicon.ico` | 站点图标，固定以 SVG 提供，可被 `--web-dir` 覆盖 |
| `GET` | `/assets/*` | 内嵌资源，可被 `--web-dir` 逐文件覆盖 |
| `GET` | `/api/nav` | 当前配置 + 版本元信息；尚未加载到配置时返回 503 |
| `GET` | `/api/status` | 探测结果；未开启探测时返回 `{"enabled":false}` 而不是错误 || `GET` | `/api/icon?h=<host>` | 图标代理。仅限配置里出现过的 host，响应带沙箱 CSP |
| `GET` | `/api/events` | SSE 流。连上先发 `hello`（带当前 revision），之后每次变更发 `changed`。并发上限 256，超出返回 503（一个标签页一条，正常远够；`/healthz` 的 `subscribers` 是当前数） |
| `POST` | `/api/webhook` | Gitea 推送回调，HMAC 校验 |
| `POST` | `/api/refresh` | 手动触发重载，需要 `Authorization: Bearer <admin-token>` |
| `GET` | `/healthz` | 健康检查，见下 |

`/healthz` 返回：

```json
{
  "status": "ok",              // ok | degraded | error | starting
  "version": "v1.0.0",
  "source": "forgejo:your-name/nav/nav.yaml@main",   // 配置还没加载成功时也会报，指向哪里一眼可见
  "revision": "3f2a1b...",     // forge 模式是 git blob SHA，文件模式是内容 sha256
  "themeRevision": "tc1b87a...", // nav.yaml + custom.css 的合成修订号
  "updatedAt": "2026-10-12T10:18:40Z",
  "groups": 3, "links": 10,    // 只统计公开条目：这个接口不鉴权
  "subscribers": 2,            // 当前挂着的浏览器/SSE 连接数
  "uptime": "3h12m",
  "goroutines": 8,             // 下面三个是给低配机器看的
  "heapMB": 0.7,
  "sysMB": 12.8,
  "probesEnabled": true,       // 开启探测时才有下面两项
  "probesUp": 10, "probesDown": 2,   // 同样只算公开条目
  "iconsCached": 8,
  // 没有 warnings 字段：见下
  "lastError": "..."           // 只在这个字段出现时才有
}
```

`status` 的含义：`ok` 一切正常 · `degraded` 页面在正常工作但 git 里的配置是坏的（**监控该盯这个**）· `error` 一份配置都没加载成功 · `starting` 刚启动还在拉。HTTP 状态码只在 `error`/`starting` 时是 503。被容忍的配置问题不影响 `status`——那是"容忍了但你该知道"，而它们**只出现在进程日志和解锁后的页面里**（见下）。

**`/healthz` 不鉴权，所以它只描述公开层**：`groups`/`links`/`probesUp`/`probesDown` 都只算公开条目，
`warnings` 这个字段根本不出现（它带 `omitempty`，公开视图里是空的）。告警文本里带着条目名
（`groups[0].links[0] (日记).hotkey: …`），放在一个可能
公网可达的接口上就等于把私密条目的名字念出来。完整告警在进程日志里，也在解锁后的页面上；
`lastError` 仍然保留，因为监控需要它判断 `degraded`。

> 配置**解析失败**时（见 [校验分级](../README.md#校验分级)），`/api/nav` 的 503 与 `/healthz` 的
> `lastError` 会给出拒绝原因，但**不会回显出错的值**——`url` 可能带着令牌，而 Go 自己
> `url.Parse` 的错误文本会把整条输入原样引用。所以报错只说明是哪种问题（不可解析 / 缺 scheme /
> 用了危险协议 / 缺 host），定位靠错误里的 `groups[i].links[j]`（或 `search.engines[i]`）坐标。

访问日志**只记路径，不记查询串**：`/api/icon` 的 query 里带着配置的链接 URL。

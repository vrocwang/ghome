# HTTP 接口

[README](../README.md) 里那份端点表在这里，附完整的响应示例。

这是一个内部导航页：绝大多数接口是给页面自己用的，你真正会手敲的只有 `/healthz`
和 `/api/refresh`。

`/api/nav` 和 `/api/status` 会看调用方是谁：请求带上 `Authorization: Bearer <能读取配置仓的令牌>`
就能拿到被 `private: true` 标记的条目；否则那些条目、以及它们的探测结果，都不会出现在响应里。
`/api/icon` 同样把关：只被私密条目使用的主机对匿名调用者返回 404，与「主机不在配置里」
不可区分。细节见 [README 的「私密链接」](../README.md#私密链接)。


| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/` | 页面本身（内嵌在二进制里，可被 `--web-dir` 覆盖） |
| `GET` | `/theme.css` | 由 `style:` 令牌生成的 CSS 变量 + 配置仓里的 `custom.css` |
| `GET` | `/favicon.ico` | 站点图标，固定以 SVG 提供，可被 `--web-dir` 覆盖 |
| `GET` | `/assets/*` | 内嵌资源，可被 `--web-dir` 逐文件覆盖 |
| `GET` | `/api/nav` | 当前配置 + 版本元信息；尚未加载到配置时返回 503 |
| `GET` | `/api/status` | 探测结果；未开启探测时返回 `{"enabled":false}` 而不是错误 |
| `GET` | `/api/icon?h=<host>` | 图标代理。仅限配置里出现过的 host，响应带沙箱 CSP |
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
  "groups": 3, "links": 10,
  "subscribers": 2,            // 当前挂着的浏览器/SSE 连接数
  "uptime": "3h12m",
  "goroutines": 8,             // 下面三个是给低配机器看的
  "heapMB": 0.7,
  "sysMB": 12.8,
  "probesEnabled": true,       // 开启探测时才有下面两项
  "probesUp": 10, "probesDown": 2,
  "iconsCached": 8,
  "warnings": ["ignored unknown field (line 17: field titel ...)"],
  "lastError": "..."           // 只在这个字段出现时才有
}
```

`status` 的含义：`ok` 一切正常 · `degraded` 页面在正常工作但 git 里的配置是坏的（**监控该盯这个**）· `error` 一份配置都没加载成功 · `starting` 刚启动还在拉。HTTP 状态码只在 `error`/`starting` 时是 503。`warnings` 存在不影响 `status`——那是"容忍了但你该知道"。

访问日志**只记路径，不记查询串**：`/api/icon` 的 query 里带着配置的链接 URL。

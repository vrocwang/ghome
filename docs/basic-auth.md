# 在 ghome 前面加 HTTP Basic 认证

README 里只留摘要。完整说明（含实测数据与 nginx 配置）在这里。


## 实测结论

**UI 和实时更新不受影响，但 webhook 会被打断。** 拿真实反代实测过：

| 链路 | 加了 Basic 之后 |
| --- | --- |
| 浏览器打开页面、`/api/nav`、`/theme.css`、图标 | ✓ 正常 |
| **SSE 实时更新**（`/api/events`） | ✓ 正常，实测 1.5s 到达且页面未重载 |
| **Gitea/Forgejo 的 webhook** | ✗ **401**，请求根本到不了 ghome |
| **私密链接的解锁**（往 `/api/nav` 发查看者令牌） | ✗ 解锁失败：令牌走 `Authorization`，被代理当成 Basic 凭据吃掉了 |
| Docker 的 `HEALTHCHECK` | ✓ 不受影响（它探的是容器内的 ghome，不经代理） |

浏览器只会在整页加载时收到**一次**认证挑战，之后把凭据复用到所有后续请求上——
包括 SSE 长连接和图标，所以 UI 侧不用管。

例外有两个：**forge 不会输密码**，**查看者令牌和 Basic 抢同一个头**。受保护的
`/api/webhook` 会把 401 返回给 forge，秒级更新静默死掉，退化成轮询（页面照样更新，只是最多
晚一分钟）——这个"静默"正是它危险的地方；受保护的 `/api/nav` 则会把 401 返回给粘贴令牌的人，
解锁永远失败。

## 修法：豁免这三条路径

而不是给它们配密码。第一条是 webhook：

```nginx
location = /api/webhook {
    auth_basic off;
    proxy_pass http://ghome;
    # ...
}
```

这么做不削弱安全性：**HMAC 密钥才是这个接口真正的鉴权**（没配
`--webhook-secret` 时它直接返回 403 不工作），实测错误签名仍然被 ghome 拒成 401。
想再加一层的话，用来源 IP 限制比用一个 forge 发不出的密码更合适。

**私密链接要另外豁免两条路径。** 查看者令牌发往 `/api/nav` 和 `/api/status`，走的也是
`Authorization` 头，而这两个接口**没有第二个令牌头可以退**（`/api/refresh` 有
`X-Ghome-Token`，私密层没有）。要让人能解锁私密条目：

```nginx
location = /api/nav    { auth_basic off; proxy_pass http://ghome; }
location = /api/status { auth_basic off; proxy_pass http://ghome; }
```

不豁免的后果是**只影响私密层**：页面照常显示公开条目，粘贴令牌会被代理的 401 挡掉，
所以解锁永远失败。这笔取舍要说清楚——`auth_basic off` 是按**路径**关的，而 `/api/nav`
正是浏览器拿导航内容的那一次请求，所以这等于**公开导航页对所有能连上代理的人不再要密码**
（内嵌页面、`/theme.css`、`/assets/` 仍在 Basic 后面，但内容的入口开了）。公开条目本来就是
给匿名访客看的，私密层的鉴权仍是"令牌 + 配置仓读权限"，所以这不是把私密条目放出去；但如果
你要的是"整页都要密码，同时私密层还能解锁"，那需要 Basic 与查看者令牌各走一个头，而 ghome
目前没有第二个头可退——这是当前实现的已知边界。完整的 nginx 片段见
[`deploy/nginx.conf.example`](../deploy/nginx.conf.example)。

另外 `/api/refresh` 有个坑：nginx 的 Basic 和 ghome 的 admin token **都用
`Authorization` 头**，一个头装不下两个值。走代理时改用 `X-Ghome-Token`：

```bash
curl -u user:pass -H "X-Ghome-Token: $GHOME_ADMIN_TOKEN" -X POST https://nav.example.com/api/refresh
```

（`Authorization: Bearer ...` 在直连时正常，经 Basic 代理时会被代理当成 Basic 凭据而失败。）

# 在 ghome 前面加 HTTP Basic 认证

README 里只留摘要。完整说明（含实测数据与 nginx 配置）在这里。


## 实测结论

**UI 和实时更新不受影响，但 webhook 会被打断。** 拿真实反代实测过：

| 链路 | 加了 Basic 之后 |
| --- | --- |
| 浏览器打开页面、`/api/nav`、`/theme.css`、图标 | ✓ 正常 |
| **SSE 实时更新**（`/api/events`） | ✓ 正常，实测 1.5s 到达且页面未重载 |
| **Gitea/Forgejo 的 webhook** | ✗ **401**，请求根本到不了 ghome |
| Docker 的 `HEALTHCHECK` | ✓ 不受影响（它探的是容器内的 ghome，不经代理） |

浏览器只会在整页加载时收到**一次**认证挑战，之后把凭据复用到所有后续请求上——
包括 SSE 长连接和图标，所以 UI 侧不用管。

webhook 是唯一的例外：**forge 不会输密码**。所以受保护的 `/api/webhook` 会把 401
返回给 forge，秒级更新静默死掉，退化成轮询（页面照样更新，只是最多晚一分钟）。
这个"静默"正是它危险的地方。

## 修法：豁免这一条路径

而不是给它配密码：

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

另外 `/api/refresh` 有个坑：nginx 的 Basic 和 ghome 的 admin token **都用
`Authorization` 头**，一个头装不下两个值。走代理时改用 `X-Ghome-Token`：

```bash
curl -u user:pass -H "X-Ghome-Token: $GHOME_ADMIN_TOKEN" -X POST https://nav.example.com/api/refresh
```

（`Authorization: Bearer ...` 在直连时正常，经 Basic 代理时会被代理当成 Basic 凭据而失败。）

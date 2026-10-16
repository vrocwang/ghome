# 安全上的取舍

[README](../README.md) 只留一句"见这里"。这份文档说明被保护的是什么、以及每一条具体怎么做的。
威胁模型是**一台放在内网或公网的小导航页**：要挡的是"访客瞄一眼就看到不该看的东西"、
"配置仓里的 token 被浏览器拿到"、"一份写错的配置变成别人浏览器里的脚本"，不是"抵抗已经登进
这台机器的人"。

## 凭据

- forge token 只存在服务端：内存或环境变量里，**不进 URL、不进日志、不进响应**。
- 每个 flag 都有同名的 `GHOME_*` 环境变量，就是为了让 token 不出现在 `ps` 里。
- 访问日志**只记路径、不记查询串**：`/api/icon` 的 query 里可能有链接自带的 token。
- 私密链接的解锁令牌也存在浏览器 `sessionStorage` 而不是 cookie：没有 cookie 就没有 CSRF 面，
  关掉标签页即登出，服务端也不需要签名或会话存储。判定按令牌的 sha256 缓存，令牌本身不落盘。
- 配置解析失败时，错误文本**既不回显出错的值、也不回显条目名**（前者可能带着令牌，而 Go 的
  `url.Parse` 错误会把整条输入原样引用；后者属于私密条目，而 `/healthz` 与 503 响应都不鉴权）。
  定位靠 `groups[i].links[j]` 这样的坐标。

## 页面与响应

- 前端全部用 DOM API 和 `textContent` 构建，没有一处 `innerHTML`，所以恶意的 `nav.yaml` 注入不了
  标记。
- 响应带 `default-src 'self'` 的 CSP，外加 `nosniff`、`X-Frame-Options: DENY`、
  `Referrer-Policy: no-referrer`（导航页会把人送到第三方站点，不泄漏自己的 URL）。
  `img-src` 放开了 `http:`/`https:`，因为配置里可以显式写图片 URL。
- `/api/icon` 的响应另带 `default-src 'none'; sandbox`：第三方 SVG 是从我们自己域名发出去的，
  不加这个，直接打开图标 URL 就能执行脚本。

## 配置能写什么

- `url` 走**危险协议黑名单**，只拦四种真能执行内容的：`javascript:`、`data:`、`vbscript:`、
  `blob:`。其余一律放行，因为内部导航页本来就会链到 `ssh://`、`vscode://`、`smb://` 这些。
- `style` 的颜色值只收 `#hex` / `rgb()` / `hsl()` / 颜色关键字，字号有范围，`font-family` 只收
  字母数字和少数标点：这些值会直接进 CSS，不校验就等于开了一个注入点。
- `style.customCss` 与 `--icon-dir` 里的文件名都经过路径白名单（不许 `..`、不许绝对路径、
  必须 `.css` 结尾），读文件时还会再解析一次符号链接、确认没跑出允许的目录。

## 写接口

- `POST /api/webhook` 没配 `--webhook-secret` 就返回 403，不工作。签名用 HMAC 校验，四种头任一种
  匹配即可：`X-Forgejo-Signature` / `X-Gitea-Signature`（裸 hex，SHA256）、`X-Hub-Signature-256`
  （`sha256=` 前缀，SHA256）、`X-Hub-Signature`（`sha1=` 前缀，**SHA1**，为兼容把 Gitea 配成 GitHub
  风格的实例而保留——它只用来决定"要不要重新拉一次配置"，不是一条内容完整性保证）。比较走
  `crypto/subtle.ConstantTimeCompare`，不是 `==`。
- `POST /api/refresh` 没配 `--admin-token` 就返回 403。admin token 与查看者令牌是两个不同的秘密，
  前者也可以用 `X-Ghome-Token` 头给（与 nginx Basic 抢 `Authorization` 时用这个）。
- 请求体有上限（webhook 1 MiB），读取有超时，签名校验之前不会无限等待。

## 图标代理不是开放代理

`/api/icon` 只服务当前配置里出现过的 host，而且**允许列表管每一跳**：服务端只请求配置里那个 host 的
`<host>/favicon.ico`，它根本不会去读页面，所以"页面把图标指到第三方"这条路径不存在；而 302 跳到别的
host 仍然会被拒绝。索引只从**公开**条目建，所以只被私密条目使用的主机对任何调用者都是 404，且与"主机
不在配置里"连耗时都一样。细节见 [icons.md](icons.md)。

代价是图标路径是哈希的站点（`/assets/favicon-a1b2c3.svg` 这类）拿不到图标，显示字母头像——用
`--icon-dir` 放一份，或用链接自己的 `icon:` 字段解决。

## 私密链接

完整说明见 README 的[私密链接](../README.md#私密链接)。要点：

- **过滤在服务端序列化之前**，未解锁的请求根本收不到那些条目；被过滤后只剩空壳的分组整个丢掉。
- 解锁判据是"这个令牌能读配置仓"：调一次 `GET /api/v1/repos/{owner}/{repo}`，要求 200、
  响应体的 `full_name` 等于这个仓、且**不跟随重定向**（否则挡在前面的登录页返回 200 就能解锁）。
- 探测结果、健康计数、图标索引、告警列表都只从公开条目出发。
- 要挡的是"访客瞄一眼就看到"：解锁之后那些 URL 就进了那个浏览器的历史与 devtools，
  它不保护目标服务本身，也不抵抗能登进这台机器的人。

## 已知取舍

- `/healthz` 与 `/api/nav` 不鉴权。前者只描述公开层（不含告警列表）；配置**解析失败**时两者都会
  给出拒绝原因，但不会回显出错的值。要放在公网，建议在反代上给这两个路径加来源限制。
- 私密条目的**数量**会以 `private.total` 发给前端（要用它显示"还有 N 个私密链接"），
  分组数与链接数只统计公开条目。

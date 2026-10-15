# 排障

[README](../README.md) 里有一张常见现象的速查表。这里是更完整的一份，按"哪里出问题"分组。

## 配置没生效

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 页面一直"配置尚未就绪" | 首次拉取就没成功 | `curl localhost:8080/healthz`，看 `lastError` |
| `gitea 401` | token 过期/被撤销 | 重新生成 token |
| `gitea 403` | token 没有这个仓的读权限 | 生成时勾 `repository: read` |
| forge 404 但文件明明在 | `--forge-ref` 或 `--forge-path` 不对 | 注意大小写；`--forge-path` 是仓内相对路径 |
| push 了但页面要等一分钟 | webhook 没通，走的轮询 | 看网页钩子的"最近投递"：成功是 **202** |
| webhook 报 401 | 密钥两边不一致 | 仓库设置里的密钥和 `--webhook-secret` 必须逐字符相同 |
| webhook 报 202 但没更新 | 推的不是被监听的分支 | 看响应体里的 `"status":"ignored"` 和 reason |
| 以为自己在跑 Gitea，其实是 Forgejo | `/api/v1/version` 报的 `16.0.5+gitea-1.22.0` 里，`+gitea-` 后是**兼容的 Gitea API 版本** | `curl 你的域名/api/forgejo/v1/version` 返回 200 就是 Forgejo |
| 页面顶部出现黄条 | 配置里有拼错的字段或写不出的颜色 | 看提示条内容，或日志里的 `config warning`（无鉴权的 `/healthz` 不带告警） |
| 改了颜色但页面没变 | 多半是 ghome 还没察觉这次推送，其次才轮到浏览器缓存 | 看进程日志里的 `config warning`；`/theme.css` 末端会写明 custom.css 的版本哈希 |
| 想改样式又不想装 Go | 不需要装 | 用 `style:` 令牌或 `custom.css`；要动结构才用 `--dump-web` + `--web-dir` |

坏配置把页面卡住时，直接 `git revert` 那次提交即可，不需要重启服务。

## 缓存文件

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 日志出现 `could not write the cache file ... permission denied` | 进程（uid 65534）对 `--cache-file` 所在**目录**没有写权限。Docker 下几乎总是 `/data` 被挂成了 root 建的 bind mount，镜像里 `chown 65534` 的 `/data` 被挂载点盖掉了 | 用 named volume（`ghome-data:/data`），或在宿主上 `install -d -o 65534 -g 65534 -m 0700 <dir>`。**注意写的是目录权限**：`writeAtomic` 是"写 `.tmp` 再 rename"，两步都要求目录可写 |
| 只看到写缓存的告警，页面却一切正常 | 缓存只影响"forge 不可达时重启还能服务上一版" | 别忽略它：真正出问题的是下一次 forge 挂掉时的重启，那时会直接 503 并被健康检查判为不健康 |

## 实时更新

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 实时更新一顿一顿的 | nginx 缓冲了 SSE | `proxy_buffering off;` + `proxy_read_timeout 1h;` |
| 加了 Basic 密码后，push 要等一分钟才生效 | webhook 被代理的 401 挡住了，退化成轮询 | 给 `/api/webhook` 加 `auth_basic off;`（HMAC 才是它真正的鉴权） |
| 加了 Basic 后 `/api/refresh` 报 401 | Basic 和 admin token 抢同一个 `Authorization` 头 | 改用 `X-Ghome-Token` 头 |
| 关了 `--poll` 之后再也不更新 | webhook 被上游挡住，而轮询已经关掉 | 先把 `--poll` 放回 `10m`，确认 webhook 稳定了再关 |
| 子路径下打开是白页 | 少了结尾斜杠 | ghome 会把 `/nav` 301 到 `/nav/`；反代也请用 `proxy_pass http://ghome/;`（结尾带斜杠） |

## 图标

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 图标都是字母头像 | 该站点没有可解析的图标，或服务端取不到 | 用链接的 `icon:` 显式指定，或把图标放进 `--icon-dir` |
| 放了本地图标却不生效 | 文件名不对（必须是 `<host>` 加扩展名，host 不含端口），或路径拼错 | 启动日志会报 `local icon directory ready, icons=N`；`N=0` 或出现 `--icon-dir is unusable` 就说明没生效 |
| 本地图标和联网结果冲突 | 本地文件优先，且排在内存缓存之前 | 这是有意的：服务端立刻改用（删掉立刻回退）；但浏览器那边还压着 7 天缓存，需要硬刷新 |
| 内网服务图标是同一个 | `/api/icon` 按 host 查，一个 host 只取配置里第一个用它的链接 | 用链接自己的 `icon:` 分别指定 |
| 换了 `--icon-dir` 里的文件，页面还是旧图标 | 图标响应带 7 天缓存**且没有 ETag/Last-Modified**，浏览器无从重验证 | 硬刷新（`Cmd/Ctrl+Shift+R`）；配置 revision 变化也会让 URL 里的 `?v=` 变，从而自动失效 |
| 某个图标 404 | 该 host 不在配置里，或它只被私密条目使用 | 这是设计内的，见 [README 的私密链接](../README.md#私密链接) |
| 所有图标都是首字母头像，日志里 iconsCached 一直是 0 | `--icon-fetch=off`，或服务端到各站点的出站被防火墙挡了 | 前者是有意的；想要真图标就放 `--icon-dir` 或给链接写 `icon:` |

## 状态灯

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 状态点一直是灰色 | 探测未开启，或该链接不可探测（非 http/https） | 确认 `status.enabled` 和该链接的 `probe` |
| 服务返回 401 但显示绿点 | 有意为之：401 说明服务活着 | 想改成红灯的话提 issue，不建议 |
| 状态灯不跟着变化 | 结果没变就不会广播 | 只有状态或状态码变化时才推事件，这是为了避免每分钟吵一次 |
| 改了个标题，探测结果全没了 | 改了 `status.interval` / `timeout` / `concurrency` 才会重建探测器 | 只改别的字段不会重建；这三个值变了才会丢弃已有结果 |

## 排查用的东西

```bash
curl -s localhost:8080/healthz | python3 -m json.tool   # status / revision / lastError
journalctl -u ghome -f                                  # config warning、webhook、探测摘要
curl -sI localhost:8080/theme.css                       # 样式表是否带上了修订号
```

`/healthz` 的 `status` 含义：`ok` 一切正常 · `degraded` 页面在正常工作但 git 里的配置是坏的
（监控该盯这个）· `error` 一份配置都没加载成功 · `starting` 刚启动还在拉。字段清单见
[http-api.md](http-api.md)。

注意 `/healthz` **不鉴权**，所以它只说公开层的事：`groups`/`links`/探测计数都只算公开条目，
响应里**没有 `warnings` 字段**（它带 `omitempty`，公开视图里是空的）。要看完整告警读进程日志，
或解锁后的页面顶部提示条。

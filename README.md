# ghome

配置托管在 git 里的导航页。**改 `nav.yaml` → `git push` → 页面自己变**，不用重新部署、不用重新构建。

- **配置就是仓库里的一个 YAML 文件**——历史、diff、review、回滚都白送。
- **改样式也不用重新编译**：颜色、圆角、字号、密度都是仓库里的数据，push 即生效。
- **三套布局**：`grid` 精致卡片 / `dashboard` 紧凑密集 / `list` 单列。
- **私有仓可用**：token 只存在服务端。
- **私密链接**：标记 `private: true` 的条目，只有拿得出配置仓读权限令牌的人才收得到。
- **图标由服务端解析并缓存**：哈希命名的图标、没有 `favicon.ico` 的内网站点也能显示。
- **可选的服务状态灯**，默认关闭，关着时零探测流量。
- **单二进制**：静态编译的 Go，空载内存约 13 MB。
- **推错配置不会把页面搞挂**：结构性问题整份拒绝，继续服务上一版。

---

## 30 秒跑起来

本地用文件模式，不需要 Gitea 就能看到效果：

```bash
make build
./bin/ghome --source=file --file=./nav.yaml --poll=1s
# 打开 http://127.0.0.1:8080/
```

现在编辑 `nav.yaml` 存盘，页面会在 1 秒内自己更新，不需要刷新。

`--poll=1s` 是给本地演示的：ghome 不用文件系统通知，它靠轮询（或 webhook、`POST /api/refresh`）发现变化，生产默认是 60s。想先看全部可用字段，读 [`nav.example.yaml`](nav.example.yaml)。

---

## 接你的私有 Gitea / Forgejo

### 先选：API 直读还是本地文件

| | `--source=forgejo` / `gitea`（推荐） | `--source=file` |
| --- | --- | --- |
| 配置从哪来 | 直接调 forge API 读文件 | 读本地文件 |
| 需要什么 | 一个只读 token | 别的工具（CI、cron `git pull`、rsync）把仓库放到机器上 |
| 谁来发现变化 | webhook（秒级）+ 轮询兜底 | 轮询，或 pull 完 `POST /api/refresh` |
| `custom.css` | 一起从仓库拉 | 读 nav.yaml 同目录的兄弟文件 |

### 1. 建一个配置仓和 token

建一个仓库放 `nav.yaml`（内容参考 [`nav.example.yaml`](nav.example.yaml)），然后建 token：**头像 → 设置 → 应用（Applications）→ 生成令牌**，权限只需要**只读的仓库权限**（`read:repository`；拿不准就勾 `repo`，ghome 只会读）。Gitea 和 Forgejo 的界面路径一样。

### 2. 跑起来

```bash
export GHOME_FORGE_TOKEN=你的token
ghome --source=forgejo \
      --forge-url=https://git.example.com \
      --forge-owner=your-name --forge-repo=homepage --forge-ref=main \
      --webhook-secret=$(openssl rand -hex 32)
```

用环境变量而不是命令行参数，是为了让 token 不出现在 `ps` 里。同一组值也可以写进从
`deploy/ghome.env.example` 抄来的文件，systemd 和 Docker 都读它。

### 3. 配 webhook（"秒级"的来源）

仓库 **设置 → 网页钩子 → 添加**，只填三项：目标 URL `https://你的域名/api/webhook`、
密钥（和 `--webhook-secret` **逐字符相同**）、触发只要"推送事件"。

不配也能用，靠 60 秒轮询，代价是"改完最多一分钟生效"。

### 4. 验证

```bash
curl -s localhost:8080/healthz          # status 应为 ok
# 改一行 nav.yaml 或 custom.css 然后 push，页面上应该 1~2 秒内出现变化
```

### Forgejo 与 Gitea

同一套代码，API 路径完全兼容，`--source` 写哪个只影响 `/healthz` 和日志里的名字。只有 webhook 的签名头不同，ghome 两种都收。push 之后发生了什么、为什么还留一条轮询，见 [`docs/architecture.md`](docs/architecture.md)。

---

## 部署

```bash
make cross                                   # 产出 dist/ghome-linux-amd64
sudo install -m 755 dist/ghome-linux-amd64 /usr/local/bin/ghome
# 抄 deploy/ghome.env.example 到 /etc/ghome/ghome.env（chmod 600，填 token）
# 抄 deploy/ghome.service 到 /etc/systemd/system/，enable --now 即可
```

Docker、nginx 反代、交叉编译、健康检查，以及环境变量在容器里的给法（两个容易踩的坑），见 [`docs/deploy.md`](docs/deploy.md)。

---

## nav.yaml 配置

最小可用：

```yaml
groups:
  - name: 开发
    links:
      - name: Gitea
        url: https://git.example.com
```

顶层字段（每一项都可省略，括号里是默认值）：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `site.title` / `subtitle` / `logo` / `footer` | `导航` / 空 | 标题、副标题、logo（emoji、短字符或图片 URL）、页脚 |
| `layout` | `grid` | `grid` / `dashboard` / `list`，见[三套布局](#三套布局) |
| `style` | 空 | 颜色、圆角、字号、密度，见[改样式不用重新编译](#改样式不用重新编译) |
| `search.enabled` / `hotkey` / `engines` | `true` / `/` / 空 | 搜索框与可选的搜索引擎（`{q}` 是必填占位符） |
| `status` | `false` | 服务状态灯，见[服务状态灯](#服务状态灯) |
| `groups[]` | 必填 | 分组，至少要有一个分组、一个链接 |

分组与链接的字段：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `groups[].name` | 必填 | 分组名 |
| `groups[].icon` / `description` / `color` | 空 | 标题前的图标、副标题、这组的主色 |
| `groups[].private` | `false` | 整组隐藏，见[私密链接](#私密链接) |
| `links[].name` / `url` | 必填 | 链接名与地址 |
| `links[].description` / `icon` / `tags` | 空 | 副标题、图标（`auto` = 服务端解析）、标签（**只在 grid 布局显示**，最多 8 个；在所有布局里都是搜索关键词） |
| `links[].hotkey` | 空 | 单字符，页面任意位置按下即跳转 |
| `links[].newTab` / `pinned` / `probe` / `private` | `true` / `false` / `true` / `false` | 新标签页打开 / 提升到置顶区 / 参与状态探测 / 只有解锁后可见 |

布尔值可以按 YAML 的写法给（`true`/`false`），也可以写成 `"false"`、`yes`、`no`、`on`、
`off`、`1`、`0`——写成字符串不会再让整份配置被拒（用 `&anchor`/`*alias` 间接引用的字符串
布尔值目前仍会被拒）。链接与分组里的文本字段两侧空白会被去掉；`site` 下的标题、副标题、
logo、页脚不去动。
`probe: false` 与"非 http(s) 链接本来就不探测"是一件事的两种来源：两者都会让该链接既不进
探测列表、也不显示状态点。

**页面上能用的键**：`/`（或你配的 `hotkey`）聚焦搜索框 · `↑` `↓` 选择 · `Enter` 打开 ·
`Esc` 清空 · `T` 切主题 · 每个链接自己的 `hotkey`（优先于全局键）· `Cmd/Ctrl+K` 搜索 ·
`Cmd/Ctrl+Enter` 用搜索引擎搜当前词。

`url` 必须带协议。**只拦四种真正能执行内容的协议**：`javascript:`、`data:`、`vbscript:`、`blob:`；
其余一律放行，因为内部导航页本来就会链到各种东西（`ssh://`、`vscode://`、`smb://`、`mailto:` …）。
主题只由用户切换（右上角开关、`T` 键，或跟随系统），没有配置项。

### 校验分级

配置里的问题分两类处理，这条规则决定了"一个错别字会不会把整页弄没"：

| | 例子 | 后果 |
| --- | --- | --- |
| **结构性问题** | YAML 语法错、缺 `url`、危险协议、搜索引擎缺 `{q}`、整份配置一个链接都没有 | **整份拒绝**。页面继续服务上一版好配置，`/healthz` 变成 `degraded` 并给出 `lastError`（只给 `groups[i].links[j]` 这样的坐标，不回显条目名或那个值——它们可能属于私密条目），改好再 push 自动恢复 |
| **表面问题** | 字段名拼错、颜色词写错、非法热键、图标既不是 URL 也不是短字符 | **只忽略那一项**，其余照常更新。页面顶部出现一条黄色提示条，进程日志里也有 `config warning` |

结构性问题会让页面撒谎（链接指向一个不存在的地方），必须拒绝；表面问题只是那一项没生效，
为一个可选字段打错字就把整个导航页停掉更糟——但也不能让你永远不知道，所以有提示条和日志。
**推了坏配置不需要重启**，`git revert` 那次提交即可回滚。

---

## 私密链接

有些链接不该让访客看到——自建日记、内部工具、带 token 的入口。给链接或整个分组加 `private: true`：

```yaml
groups:
  - name: 我的私密
    private: true            # 整组隐藏，连分组名一起
    links:
      - {name: 日记, url: https://diary.example.com}
  - name: 混合
    links:
      - {name: 公开的, url: https://example.com}
      - {name: 私密的, url: https://secret.example.com, private: true}
```

**过滤在服务端做**：未解锁的请求根本收不到这些条目。发给浏览器再让前端藏起来是不行的，任何人
`curl` 一次就全拿到了；被过滤后只剩空壳的分组会整个丢掉，否则分组名本身就泄露了。

**解锁用「能读配置仓的 Forgejo 令牌」**，不需要第二份用户名单：点右上角的锁、粘贴令牌，ghome
拿它调一次 `GET /api/v1/repos/{owner}/{repo}`，必须是 200、响应体里的 `full_name` 还得等于这个仓、
且不跟随重定向。判定结果按令牌缓存（成功 5 分钟、失败 30 秒），所以**撤销最多 5 分钟后生效**。
令牌存在浏览器 `sessionStorage`，关掉标签页即失效。

几条边界：

- 它只决定给不给你看，不保护目标服务本身——日记服务自己的登录还得自己配。解锁后那些 URL 就进了
  那个浏览器的历史，挡的是"访客瞄一眼就看到"。
- 探测结果、健康计数、图标索引都只从公开条目出发；无鉴权的 `/healthz` 也不带告警列表。代价是解锁后
  私密链接显示**字母头像**，因为 `<img>` 没法携带令牌。
- 文件模式没有可询问的 forge，私密项对**所有人**都不外发，启动时会有一条 warning 说明原因；
  配置仓本身是公开仓时这套权限也不成立（公开仓里任何登录用户都能读）。
- 令牌不对时页面仍旧是公开层的样子，只给一条提示；请求本身失败时提示"暂时无法确认令牌"，不会清掉
  刚输入的令牌。

---

## 改样式不用重新编译

样式是数据，不是代码。按需要改的深度选一层。

**第 1 层：`nav.yaml` 里的 `style:` 令牌**（覆盖绝大多数需求）

```yaml
style:
  accent: "#7c3aed"       # 主色
  background: "#faf9ff"   # 页面底色
  surface: "#ffffff"      # 卡片底色
  text: "#1a1a1f"
  muted: "#6b6b78"
  border: "#e8e6f0"
  font: "Inter, system-ui, sans-serif"
  radius: 20              # 圆角
  fontSize: 15            # 正文字号
  width: 1280             # 内容最大宽度
  iconSize: 22            # 图标尺寸
  density: compact        # comfortable | compact
  cardStyle: strong       # soft | flat | strong
```

这些令牌由 `/theme.css` 以 CSS 变量发给浏览器，页面所有颜色、间距、圆角都从它们派生，所以改一个
`accent` 整页都跟着变，**包括已经开着的页面**。颜色只接受 `#hex` / `rgb()` / `hsl()` / 颜色关键字，
字号必须在 `11~24`，越界的值会被忽略并在页面顶部提示——`style` 的值会直接进 CSS，不校验就等于开了
一个注入点。

**第 2 层：`custom.css`**（想写具体规则时）

在同一个仓库里放一个样式表，然后 `style.customCss: "custom.css"` 指过去。内容会追加到 `/theme.css`
后面，也就是内嵌样式表和所有令牌**之后**，天然优先级更高，同样 push 即生效。文件最大 **512KB**
（超了会被忽略并给告警，继续用上一份样式）；路径按**仓库根**解析——`--forge-path=config/nav.yaml`
时 `customCss: "custom.css"` 指的是仓库根的 `custom.css`，文件模式下才按 nav.yaml 所在目录解析。
起点见 [`custom.css.example`](custom.css.example)，里面列了所有可用变量。

**第 3 层：`--dump-web` + `--web-dir`**（要动 DOM 结构或基础样式表时）

```bash
ghome --dump-web=/srv/ghome/web          # 把内嵌前端导出来
vim /srv/ghome/web/assets/style.css      # 随便改
ghome --web-dir=/srv/ghome/web ...       # 指过去
```

`--web-dir` 是**逐文件回退**的：目录里有就用磁盘上的，没有就用内嵌的。所以你只需要放想改的那一个
文件，升级 ghome 时没被覆盖的文件自动跟着更新。**第 4 层**是直接改 Go 代码，只有要加全新视图或新
交互时才需要。

前端默认内嵌，是为了不存在"资源和二进制版本对不上""文件缺失""只传了一半"这类故障；而"改个颜色要
装 Go"由上面三层解决，它不需要重新编译。

---

## 三套布局

在 `nav.yaml` 里改一行，push 就换：

![grid 布局](docs/layout-grid.png)

- **`grid`**（默认）每组一张卡，描述和标签都显示，适合十几到几十个入口
- **`dashboard`** 行高更小、分组带顶部色条、隐藏描述，适合上百个入口
- **`list`** 每组一行、链接横向排成芯片，最省纵向空间

三套共用同一份内容，列宽、密度、色条的差异，以及不同视口下的实测列数见
[`docs/layouts.md`](docs/layouts.md)。不管选哪套，搜索、键盘选择、置顶区、状态灯、分组配色都一样。
`pinned: true` 的链接会被提升到页面顶部的置顶区，并且不再出现在原分组里。

---

## 服务状态灯

默认**关闭**，关着的时候服务端一个探测请求都不发。

```yaml
status:
  enabled: true
  interval: 60s      # 最小 5s
  timeout: 5s
  concurrency: 8     # 最大 64
```

打开之后，每个可探测的链接右侧出现状态点（绿=在线，红=无法访问，灰=未知），右上角出现"N/M 在线"
的胶囊，点它只看异常的服务。只探测 `http`/`https` 链接，单个链接可以 `probe: false` 退出探测。

判定口径是有意的：`2xx`/`3xx`/`401`/`403` 都算**在线**（返回 401 说明服务活着，只是要登录），其余
算无法访问；先发 `HEAD`，遇到 `405`/`501` 退回 `GET`。状态变化才通过 SSE 推给浏览器，安静的时候不
产生任何事件。

---

## 图标是怎么来的

页面不让浏览器直接去第三方站点取 `favicon.ico`，而是走 `/api/icon?h=<host>`，由服务端解析和缓存。
查找顺序是 `--icon-dir` 本地文件 → 服务端内存缓存 → 联网解析 → 浏览器缓存，所以它不是每次访问都去抓。
联网那一层会解析页面里的 `<link rel="icon">`（含哈希命名的图标），没有就退到 `<host>/favicon.ico`，
最后校验返回的确实是图片；解析不到时前端回退到字母头像。

内网服务大多没有 favicon、DNS 也可能解析不了，所以本地图标目录是可靠解：文件名是 `<host>` 加扩展名，
host **转小写并剥掉端口**，所以 `nas:8080` 和 `nas:9090` 都找 `nas.svg`。

```bash
ghome --icon-dir=/var/lib/ghome/icons
```

一个 host 只有一个图标（按配置里第一个用它的链接解析），想同机的不同服务各有图标就用链接自己的
`icon:` 字段。解析顺序的完整说明、本地目录结构、缓存参数、`/api/icon` 的安全边界与实测数据见
[`docs/icons.md`](docs/icons.md)。

---

## 命令行与环境变量

每个 flag 都有一个同名的 `GHOME_*` 环境变量，**这样 token 不会出现在 `ps` 里**。优先级：命令行 flag > 环境变量 > 默认值。

| flag | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `--listen` | `GHOME_LISTEN` | `:8080` | 监听地址。建议 `127.0.0.1:8080` 交给 nginx |
| `--base-path` | `GHOME_BASE_PATH` | | 挂在子路径下，例如 `/nav`。webhook 地址也跟着变 |
| `--log-level` | `GHOME_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `--source` | `GHOME_SOURCE` | `file` | `file` / `gitea` / `forgejo` |
| `--file` | `GHOME_FILE` | `nav.yaml` | `file` 模式下的路径 |
| `--forge-url` | `GHOME_FORGE_URL` | | forge 根地址，支持反代子路径 |
| `--forge-owner` | `GHOME_FORGE_OWNER` | | 用户名或组织 |
| `--forge-repo` | `GHOME_FORGE_REPO` | | 仓库名 |
| `--forge-ref` | `GHOME_FORGE_REF` | `main` | 分支、tag 或 commit SHA |
| `--forge-path` | `GHOME_FORGE_PATH` | `nav.yaml` | 仓内路径 |
| `--forge-token` | `GHOME_FORGE_TOKEN` | | 只读 token（私有仓必填），以 `Authorization: Bearer` 发送 |
| `--poll` | `GHOME_POLL` | `60s` | 兜底轮询间隔。**`0` 关闭轮询**，只靠 webhook 和手动刷新 |
| `--webhook-secret` | `GHOME_WEBHOOK_SECRET` | | `POST /api/webhook` 的 HMAC 密钥；留空则关闭该接口 |
| `--branch` | `GHOME_BRANCH` | | 只对这个分支的推送重载；默认取 `--forge-ref` |
| `--admin-token` | `GHOME_ADMIN_TOKEN` | | `POST /api/refresh` 的令牌；留空则关闭该接口 |
| `--cache-file` | `GHOME_CACHE_FILE` | | 持久化最后一份可用配置（含仓库里的 `custom.css`），重启时 forge 不可达也能服务 |
| `--web-dir` | `GHOME_WEB_DIR` | | 从目录提供 UI，逐文件回退到内嵌副本 |
| `--icon-dir` | `GHOME_ICON_DIR` | | 本地图标目录，优先于联网抓取 |
| `--dump-web` | | | 把内嵌前端导出到目录后退出（`--web-dir` 的起点） |
| `--healthcheck` | | | 请求自己的 `/healthz`，200 退出 0；给容器健康检查用 |
| `--version` | | | 打印版本后退出 |

最后三个是**一次性动作**而不是配置，所以没有环境变量——这也是它们在 Docker 里要当命令参数给的原因。

`--poll=0` 关掉轮询：配置只在启动时读一次，之后靠 webhook 或 `POST /api/refresh` 更新。代价是**不再有
兜底**——webhook 要是被上游挡住（比如被反代的 Basic 认证拦了），页面就再也不会更新，而 `/healthz`
还会一直报 `ok`。所以先用 `10m` 这类宽松值过渡，确认 webhook 稳定了再彻底关掉。非法值的处理两条路
不一样：`GHOME_POLL=0.5` 打一条警告并回退到 60s，`--poll=0.5` 是启动即报错退出。

HTTP 接口（含 `/healthz` 的完整返回）见 [`docs/http-api.md`](docs/http-api.md)。日常只有两个会手敲：
`GET /healthz` 看状态，`POST /api/refresh` 手动触发重载（需要 `--admin-token`）。

---

## 排障

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 页面一直"配置尚未就绪" | 首次拉取就没成功 | `curl localhost:8080/healthz` 看 `lastError` |
| `gitea 401` / `403` | token 过期、撤销，或没有这个仓的读权限 | 重新生成，权限勾 `repository: read` |
| forge 404 但文件明明在 | `--forge-ref` 或 `--forge-path` 不对 | 注意大小写；`--forge-path` 是仓内相对路径 |
| push 了但页面要等一分钟 | webhook 没通，走的轮询 | 网页钩子的"最近投递"：成功是 **202**，401 是密钥不一致 |
| webhook 报 202 但没更新 | 推的不是被监听的分支 | 看响应体里的 `"status":"ignored"` 和 reason |
| 页面顶部出现黄条 | 配置里有拼错的字段或写不出的颜色 | 看提示条内容，或日志里的 `config warning` |
| 图标都是字母头像 | 站点没有可解析的图标，或服务端取不到 | 用 `icon:` 显式指定，或把图标放进 `--icon-dir` |
| 放了本地图标却不生效 | 文件名或路径不对（必须是 `<host>` 加扩展名，host 不含端口） | 启动日志会报 `local icon directory ready, icons=N` |
| 状态点一直是灰色 | 探测未开启，或该链接不可探测（非 http/https） | 确认 `status.enabled` 和该链接的 `probe` |
| 实时更新一顿一顿的 | nginx 缓冲了 SSE | `proxy_buffering off;` + `proxy_read_timeout 1h;` |
| 加了 Basic 后 webhook 失效 | 代理的 401 挡住了 forge | 给 `/api/webhook` 加 `auth_basic off;` |
| 子路径下打开是白页 | 少了结尾斜杠 | 用 `/nav/`；反代用 `proxy_pass http://ghome/;`（结尾带斜杠） |

更多症状（图标缓存冲突、状态灯判定、容量上限、`/healthz` 字段含义）见
[`docs/troubleshooting.md`](docs/troubleshooting.md)。

**排查工具**：`curl -s localhost:8080/healthz | python3 -m json.tool` 看状态、`lastError` 和
`revision`；`journalctl -u ghome -f` 看 `config warning`、webhook 与探测摘要。

---

## 设计上的几个取舍

**为什么 webhook 之外还留轮询？** webhook 是尽力而为的：forge 重启、网络抖动、密钥改错、钩子被误删，
任何一种都会让更新静默停掉，而静默停掉是最难发现的故障。轮询是那份保险。

**为什么坏配置不覆盖好配置？** 导航页是入口，挂掉影响所有人。宁可让一个人看到自己的改动没生效
（`/healthz` 里明写着原因），也不能让所有人打开是一片错误页。

**为什么样式不内嵌成代码？** 样式降级成数据（令牌 + `custom.css`）之后，改颜色不需要装 Go、也不需要
重新部署；而前端保持内嵌，是为了不存在资源和二进制版本对不上这类故障。

安全上的取舍（token 的存放、CSP、路径与协议白名单、两个写接口默认关闭、签名比较方式）见
[`docs/security.md`](docs/security.md)。

---

## 开发

```bash
make check    # gofmt + go vet + go test
make race     # 带竞态检测
make run      # 用 ./nav.yaml 跑起来
make ui-check # 无头 Chrome 驱动界面（需要先跑起一个实例）
```

前端没有构建步骤也没有 Go 测试，所以界面回归靠 `scripts/ui-check.mjs`（22 项断言；配置里没有
置顶条目时跳过 2 项，任一失败即非 0 退出）。它需要 Chrome，因此不在 `go test` 里——那条路径
保持零外部依赖。代码结构与测试覆盖范围见
[`docs/development.md`](docs/development.md)。

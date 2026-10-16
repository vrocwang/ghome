# 部署

[README](../README.md) 里只有 systemd 的最短路径。这里是完整的一份：systemd、Docker、
nginx 反代、交叉编译，以及在容器里给参数的两个坑。

## systemd

```bash
make cross                                        # 产出 dist/ghome-linux-amd64
sudo install -m 755 dist/ghome-linux-amd64 /usr/local/bin/ghome
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ghome
sudo mkdir -p /etc/ghome
sudo cp deploy/ghome.env.example /etc/ghome/ghome.env
sudo chmod 600 /etc/ghome/ghome.env               # 里面有 token
sudo editor /etc/ghome/ghome.env
sudo cp deploy/ghome.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now ghome
journalctl -u ghome -f
```

[`deploy/ghome.service`](../deploy/ghome.service) 里带了 `MemoryMax=96M`、`ProtectSystem=strict`、
`NoNewPrivileges` 等一整套加固，以及 `StateDirectory=ghome`（缓存文件要写这里，
`ProtectSystem=strict` 下只有这个目录可写）。

## Docker

镜像分两阶段：`golang:alpine` 编译，然后只把**静态二进制 + CA 证书**拷进 `scratch`。最终
镜像约 7 MB，没有 shell、没有包管理器、没有 libc；以非 root（`65534`）运行。

```bash
make docker            # = docker build --build-arg VERSION=$(git describe ...) -t ghome .

docker run -d --name ghome \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v ghome-data:/data \
  -e GHOME_SOURCE=forgejo \
  -e GHOME_FORGE_URL=https://git.example.com \
  -e GHOME_FORGE_OWNER=your-name -e GHOME_FORGE_REPO=homepage \
  -e GHOME_FORGE_TOKEN=... \
  -e GHOME_WEBHOOK_SECRET=... \
  ghome
```

镜像自带的默认参数是 `--listen=:8080 --cache-file=/data/cache.yaml`，所以上面不用重复写。

> **坑一：给 `docker run` 传任何参数会整体替换掉这些默认值。**
> `docker run ghome --source=file` 会丢掉 `--listen`，容器起来就不监听任何端口。要覆盖就写全：
> `docker run -p 8080:8080 ghome --listen=:8080 --cache-file=/data/cache.yaml --source=file`

> **坑二：优先用环境变量，别用命令行参数。**
> 镜像的 `HEALTHCHECK` 跑的是 `/ghome --healthcheck`，它**不继承**你的 CMD 参数，只读环境变量。
> 所以如果你用 `--listen=:9000` 或 `--base-path=/nav` 这样的参数去改地址，健康检查仍会去探
> 默认的 `:8080/healthz`，结果永远是不健康。用 `GHOME_LISTEN` / `GHOME_BASE_PATH` 就没有这个
> 问题，而这也正是十二要素应用的推荐姿势。

### Compose（可选）

见 [`deploy/docker-compose.example.yml`](../deploy/docker-compose.example.yml)：

```bash
cp deploy/ghome.env.example deploy/ghome.env   # 填 forge 设置和密钥
docker compose -f deploy/docker-compose.example.yml up -d --build
```

### 健康检查

`scratch` 里没有 curl 也没有 shell，所以镜像的 `HEALTHCHECK` 调的是二进制自己：
`ghome --healthcheck` 去请求 `/healthz`，**只有 HTTP 200 才算健康**。503（进程活着但配置还
没加载出来）会被判为不健康——那正是重启该解决的问题，而单纯探端口会把它当成正常。

```bash
docker run --rm ghome --healthcheck          # 用默认 listen 地址
docker exec ghome /ghome --healthcheck       # 容器里查自己
```

### 卷

`/data` 是给 `--cache-file` 的，挂上之后重启期间哪怕 forge 连不上也还能服务上一版页面——
配置和仓库里的 `custom.css` 会一起从缓存恢复（写在 `cache.yaml` 旁边的 `cache.yaml.css`），
所以样式不会在这段时间里悄悄消失。

`--web-dir` / `--icon-dir` 需要把宿主目录以只读挂进去
（`-v ./web:/srv/web:ro -e GHOME_WEB_DIR=/srv/web`），注意容器里的进程是 uid `65534`，
宿主目录得给它读权限（一般 0755 就行）。

### 构建时的那条 WARNING

如果构建输出里有 `WARNING: current commit information was not captured by the build`，可以忽略：
构建会成功，镜像也能用。它最常见的成因是**用普通用户 clone、用 root 构建**（git 的 dubious
ownership 检查），三种解法见 [`docker-build-warning.md`](docker-build-warning.md)。

### 多架构

Dockerfile 用 `--platform=$BUILDPLATFORM` 让编译器保持宿主架构，所以是交叉编译而不是模拟：

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t ghome --push .
```

普通 `docker build` 也照常工作（`GOOS`/`GOARCH` 为空时 Go 回退到宿主）。

### 这些参数在容器里怎么给

21 个 flag 在容器里分三类，给法不一样：

| 类型 | 有哪些 | 怎么给 |
| --- | --- | --- |
| **设置** | 18 个：`--listen` `--source` `--file` `--forge-*`（6 个）`--poll` `--webhook-secret` `--admin-token` `--base-path` `--cache-file` `--web-dir` `--icon-dir` `--icon-fetch` `--log-level` | 环境变量，写进 `ghome.env`，compose 用 `env_file` 引入 |
| **需要宿主目录** | `--web-dir`、`--icon-dir` | 环境变量 **+ 只读卷**，两半必须指向同一个容器内路径 |
| **一次性动作** | `--dump-web`、`--healthcheck`、`--version` | 直接当命令参数——它们是动作不是配置，所以**没有环境变量** |

完整对照见 README 的[命令行与环境变量](../README.md#命令行与环境变量)表；
`deploy/ghome.env.example` 里每一项都写好了，包括被注释掉的可选项。

**需要挂目录的两个**，两半必须成对：

```yaml
environment:
  GHOME_ICON_DIR: /srv/icons
volumes:
  - ./icons:/srv/icons:ro
```

`--icon-dir` 用不了时启动会打一条明确的 WARNING，不会静默失效。

**`--dump-web` 是一条命令，不是配置**：

```bash
docker run --rm -u "$(id -u):$(id -g)" \
  -v "$PWD/web:/out" ghome --dump-web=/out
```

`-u` 是关键：镜像默认 uid 65534，而 bind mount 属于你，不换 uid 会直接 `permission denied`
（报错是 `mkdir /out/assets: permission denied`）。`--web-dir` 是**逐文件回退**的，所以导出后
只需要改你想改的那个文件，删掉的文件继续跟着二进制走。

**`--healthcheck` 由镜像的 HEALTHCHECK 自己调用**，正常情况下不用管。要在 compose 里覆盖，
注意它**不继承 CMD 参数、只读环境变量**（上面那个坑二）：

```yaml
healthcheck:
  test: ["CMD", "/ghome", "--healthcheck"]
```

**不用 Docker 也行。** `make cross` 产出的就是同一个静态二进制，`scp` 上服务器直接跑，
连容器运行时都省了。

## nginx 反代

见 [`deploy/nginx.conf.example`](../deploy/nginx.conf.example)。**最关键的两行是
`proxy_buffering off;` 和 `proxy_read_timeout 1h;`**——不关缓冲，SSE 会成块到达，实时更新
看起来就像坏了。

### 加了密码（auth_basic）之后

UI 和实时更新不受影响，但 **webhook 会被 401 挡住、退化成轮询**（页面照样更新，只是最多晚
一分钟——这正是它危险的地方）。修法是给 `/api/webhook` 加 `auth_basic off;`，HMAC 才是那个
接口真正的鉴权。完整的实测数据、配置片段，以及 `/api/refresh` 与 Basic 抢 `Authorization`
头的处理，见 [`basic-auth.md`](basic-auth.md)。

### 子路径

用 `--base-path=/nav` 时，反代要写 `proxy_pass http://ghome/;`（**结尾带斜杠**），否则
`/nav/assets/...` 会被原样转给 ghome，而 ghome 看到的是带前缀的路径。

## 交叉编译

```bash
make cross   # linux/amd64, linux/arm64, darwin/arm64, darwin/amd64 → dist/
```

全部静态链接（`file` 会告诉你 `statically linked`），拷到服务器上直接跑。

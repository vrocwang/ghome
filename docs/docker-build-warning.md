# 构建时的 `current commit information was not captured` 警告

这是 [README](../README.md) 里那条构建警告的完整说明——正文只留摘要，因为绝大多数
构建不会碰到它。


如果你构建时看到这条，它无害——构建会成功，镜像也能用：

```text
WARNING: current commit information was not captured by the build: failed to read
current commit information with git rev-parse --is-inside-work-tree
```

但它有几个容易搞错的点，我第一版就写错了两处，所以值得讲清楚。

它**不是** `.dockerignore` 引起的。buildx 在 `build/git.go` 里判断的是：

```go
if !gitc.IsInsideWorkTree(ctx) {
    if st, err := os.Stat(path.Join(wd, ".git")); err == nil && st.IsDir() {
        return nil, errors.New("failed to read current commit information with git rev-parse --is-inside-work-tree")
    }
    return nil, nil      // 没有 .git 就静默跳过，不报警
}
```

`wd` 是**宿主上你传给 `docker build` 的那个上下文目录**，`os.Stat` 跑在宿主上。
`.dockerignore` 只影响送进 builder 的内容，管不到这个判断。

所以触发条件是：**`.git` 在，但 git 在那个目录里拒绝把它认作工作树**。
这是个 git 问题，不是 Docker 问题。

## 最常见的成因：clone 用普通用户，构建用 root

```console
$ git clone https://github.com/vrocwang/ghome.git   # 文件属于普通用户
$ sudo docker build -t ghome .                       # 进程是 root
```

仓库属主和构建用户不一致，git 的 dubious ownership 检查（CVE-2022-24765 的
缓解措施）就会拒绝，于是 buildx 打那条警告。三个选择，按推荐顺序：

## 办法 1：别用 root 构建（真正的修法）

```bash
# Linux：把你自己加进 docker 组，然后重新登录
sudo usermod -aG docker "$USER"

# macOS + Docker Desktop：本来就不需要 sudo，去掉它即可
docker build -t ghome .
```

## 办法 2：告诉 root 的 git 这个目录是安全的（workaround）

```bash
sudo git config --global --add safe.directory "$PWD"
```

注意这是写进 **root 的** `~/.gitconfig`，因为跑 git 的是 root。

先在上下文目录里自己复现一次，能直接看到真实原因：

```bash
git rev-parse --is-inside-work-tree
git status        # 上一条失败时，这条会说清为什么
# → fatal: detected dubious ownership in repository at '...'
```

## 办法 3：让 buildx 干脆别查

`build/git.go` 开头有 `if !setGitLabels && !setGitInfo { return nil, nil }`，
而 `setGitInfo` 默认 `true`，用 `BUILDX_GIT_INFO=false` 即可跳过整趟查询。
`make docker` 已经这么设了——变量是在 Makefile 的 recipe 里设的，所以
`sudo make docker` 也照样生效：

```bash
make docker          # 或 sudo make docker
```

之所以设它不心虚：版本号和 revision 是 Dockerfile 用 `--build-arg` 显式传进去的，
buildx 那套自动推断对这个项目本来就是多余的。

顺带说一下版本号：Dockerfile 用 `--build-arg VERSION` 显式注入 `-ldflags
"-X main.version=..."`，所以镜像里 `ghome --version` 和 `/healthz` 的 `version`
字段报的是真实版本，不是 `dev`。不传的话就是 `dev`。

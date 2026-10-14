# 开发

```bash
make check    # gofmt + go vet + go test
make race     # 带竞态检测
make run      # 用 ./nav.yaml 跑起来
make cross    # 交叉编译
make ui-check # 无头 Chrome 驱动界面（需要先跑起一个实例）
```

## 代码结构

```text
main.go                      flag/环境变量解析、组装、优雅退出、探测器监管
internal/nav/                nav.yaml 的结构定义、默认值、校验、warning 收集
internal/source/             配置来源：Gitea API / 本地文件，含同仓兄弟文件读取
internal/store/              当前配置的持有者 + 订阅广播（坏配置不覆盖好配置）
internal/watcher/            轮询 + webhook 快路径 + 指数退避 + 防抖
internal/icons/              图标解析（<link rel=icon>）+ LRU 缓存 + 单飞去重
internal/status/             服务健康探测（并发受限、超时、变化检测）
internal/server/             HTTP 层（页面、API、SSE、webhook、/theme.css、健康检查）
internal/server/web/         内嵌前端（原生 JS，无框架无构建步骤）
scripts/ui-check.mjs         无头 Chrome 的界面冒烟检查
deploy/                      systemd / nginx / 环境变量样例
docs/                        专题文档与布局截图
custom.css.example           配置仓里自定义样式表的起点
```

## 前端怎么测

前端的 bug 读代码看不出来——健康胶囊没渲染、状态灯卡在"探测中"、列表布局图标自己被擦掉、
置顶项明明显示着却提示"没有匹配"，这几个都是把页面塞进无头 Chrome 实际驱动一遍才发现的。

```bash
make run &                              # 或任何跑着的 ghome
make ui-check                           # = node scripts/ui-check.mjs http://127.0.0.1:8080/
make ui-check UI_URL=http://127.0.0.1:8080/
```

它检查 20 项：无未捕获异常、无 console 错误（`/api/icon` 的 404 属于设计内降级，会被跳过并
计数）、页面结构完整、配置已渲染、骨架屏已消失、设计令牌解析成功、样式表带版本号、置顶项没有
在分组里重复、搜索过滤与空态正确（**包括只匹配到置顶项时不误报空态**这条回归）、清空后恢复。
任一断言失败就以非 0 退出。它需要 Chrome，所以**不在 `go test` 里**——那条路径保持零外部依赖。

## 测试覆盖

测试盯的是容易出错的地方：YAML 校验与 warning 分级（含恶意 URL、CSS 注入、路径穿越）、
坏配置不覆盖好配置、webhook 签名与分支过滤、SSE 端到端推送、退避上限、图标解析与缓存
（含 SSRF 边界与单飞）、探测判定口径与变化检测、`--web-dir` 的逐文件回退、私密链路的过滤
（含令牌验证缓存与计时侧信道）。

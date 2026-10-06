# BBDown 原生版（BBDown-Native）

B 站视频下载器，**纯 Go 单文件实现**，双击即用，不装任何东西。

> 与同目录下的老版 `BBDown-GUI/` 并存，互不影响。

## 为什么重写

老版能跑，但它是一个「外壳」：界面靠内嵌浏览器（pywebview + 网页前端），下载靠外部 `BBDown.exe`，
封装靠外部 `ffmpeg`。用户拿到手要凑齐 Python、WebView2 运行时和几个第三方二进制才能用。

本版的目标只有一个：**一个 exe，双击即用，不装任何东西。**

| | 老版 `BBDown-GUI/` | 本版 `BBDown-Native/` |
|---|---|---|
| 后端语言 | Python 3 | **Go（纯 Go 编译链，不需要 MSVC/gcc）** |
| 界面 | pywebview 内嵌网页 | **同一个网页，用系统默认浏览器打开** |
| 运行时依赖 | Python 3 + pywebview + WebView2 | **无** |
| 下载 | 调用外部 `BBDown.exe` | 内置 Go 实现 |
| 封装 | 调用外部 `ffmpeg` | **纯 Go 手写 fMP4 → MP4 封装器** |
| 交付形态 | 一个目录 + 若干个组件 | **单个 exe，约 12 MB** |

界面部分**沿用老版那套网页**（`internal/server/web/`，`style.css` 从老版原样搬来），
所以观感和老版一致；换掉的只是它下面的引擎。

## 快速开始

1. 双击 `dist/BBDown原生版.exe`
2. 浏览器自动打开控制面板（没自动开就用控制台打印的地址手动访问）
3. 粘贴分享文案（或短链 / BV 号 / 番剧 ep·ss）→ 点「解析视频」
4. 选画质与编码 → 点「开始下载」

首次运行会在 **exe 同级目录**生成两样东西：

- `config.json` —— 设置与登录凭证。**内含明文 SESSDATA / access_token，不要外传、不要提交到 git。**
- `downloads/` —— 默认下载目录

未登录时画质上限是 480P；点「扫码登录」用手机 B 站扫一次，即可解锁 1080P / 4K。

## 功能现状

**已完成（核心闭环）**

- 地址解析：分享文案、短链、`BV`/`av` 号、番剧 `ep`/`ss`，多地址保序去重
- 画质与编码选择（画质列表按视频实际可用项动态生成）
- 分P 选择（留空=全部，支持 `1` 或 `1,3-5`）
- DASH 分片并发下载，进度 / 速度 / 剩余时间汇总成一条进度条
- 纯 Go 封装为可直接播放的渐进式 MP4
- 扫码登录（Web 端 + TV 端两条通路）与账号校验
- 配置持久化、运行日志、明暗双主题

**尚未实现（计划中）**

弹幕、字幕、封面（下载到本地）、更多登录方式。

## 架构

```
cmd/bgui/           入口：单实例探测 → 起服务 → 拉系统浏览器
internal/server/    HTTP 服务层
  ├─ server.go        路由、中间件、空闲自杀
  ├─ handlers.go      status/verify/config/extract/parse/clipboard/open/…
  ├─ login.go         扫码登录的会话管理
  ├─ platform.go      剪贴板、目录选择框、打开浏览器（走 PowerShell，零 unsafe）
  └─ web/             ← 前端（go:embed 进 exe）
       index.html · app.js · style.css · favicon.ico
internal/app/       配置持久化、任务编排（解析 → 下载 → 封装）
internal/bilibili/  接口层：地址提取、WBI 签名、playurl、扫码登录、账号校验
internal/download/  DASH 分片并发下载与进度统计
internal/mux/       fMP4 解析 → 渐进式 MP4 组装 → 产物结构校验
internal/gui/       备用的原生 Win32 窗口（`-ui native`），默认不用
assets/             应用图标与 manifest
tools/              构建环境脚本与验证工具
```

数据流：

```
分享文案 ──extract_targets──▶ BV / ep / av ──view 接口──▶ 标题 · 分P · 时长
                                                    │
                                              playurl（WBI 签名）
                                                    │
                          ┌─────────────────────────┴─────────────────────────┐
                     视频 m4s（fMP4）                                    音频 m4s（fMP4）
                          │                                                   │
                  并发分片下载 + Range 探测体积                        （同一条进度汇总）
                          └─────────────────────────┬─────────────────────────┘
                                              纯 Go 封装器
                                解析 moov / moof / trun → 重建 stbl 采样表
                                                    │
                                              〈标题〉.mp4
```

### 界面为什么是「本地服务 + 系统浏览器」

不用内嵌 WebView2，是因为那是用户机器上唯一一个不保证存在的运行时。改成：

```
BBDown原生版.exe ──监听 127.0.0.1:18230（被占用则顺延）──▶ 系统默认浏览器
        │
        └─ 界面关闭 / 空闲超时 → 进程自己退出（windowsgui 的 exe 没有窗口可关）
```

- 地址写进 `%TEMP%/bbdown-native-<exe路径哈希>.url`。
  双击第二次时先探活这个地址，**复用已有实例并只把浏览器指过去**，
  避免起两个互不相干的服务、用户分不清哪个在下载。
- 只 bind `127.0.0.1`，不对外暴露。
- 页面 `pagehide` 时发一个 `/api/bye`，服务等 6 秒后收摊；另有 3 分钟空闲兜底
  （有任务在跑时绝不退出）。

## 构建

```bash
source tools/goenv.sh                      # 设置 GOROOT/GOPATH/CGO_ENABLED=0 等
go build -trimpath -ldflags "-s -w -H=windowsgui" -o "dist/BBDown原生版.exe" ./cmd/bgui
```

- 需要 Go 1.26+；`CGO_ENABLED=0`，纯 Go 编译链，**不需要 MSVC 或 gcc**。
- `-H=windowsgui` 去掉控制台黑框。副作用是**没有 stdout**，
  所以调试时请用 `go run ./cmd/bgui -no-open`（那样才有控制台）。
- 图标与 manifest（`assets/`）通过 `rsrc` 烘焙进 `cmd/bgui/rsrc.syso`，该文件已入库；
  改动 `assets/` 后需重新生成：

  ```bash
  go install github.com/akavel/rsrc@latest
  rsrc -manifest assets/app.manifest -ico assets/icon.ico -o cmd/bgui/rsrc.syso
  ```

## 验证

| 命令 | 作用 |
|---|---|
| `go test ./...` | 单元测试（地址提取、cookie 归一化、TV 签名、`elst` 重建、前端 id 交叉校验等） |
| `go vet ./...` | 静态检查 |
| `go run ./cmd/e2e -url "<分享文案>" -out <目录>` | 无界面端到端：解析 → 取流 → 下载 → 封装 → 结构校验 |
| `go run ./cmd/e2e` | 离线：只用 `.devdata/` 里的样例做封装回归 |
| `node tools/uitest.mjs [地址] [解析截图.png] [下载截图.png] [--download]` | 界面冒烟：无头 Edge/Chrome **真的填地址、真的点按钮**，断言渲染结果并截图 |

封装正确性的判定口径是**逐包比对**，不是「能播放就行」：
产物与 `ffmpeg -c copy` 自制参照做 `framecrc` 三级 diff（视频 / 音频 / packet），要求全空。

界面正确性的判定口径是**接口与 DOM 数字**，不是截图观感 ——
脚本会断言解析出的标题 / 画质 / 进度百分比，截图只用于人工复核观感。

## 关键约束（改动前请先读）

1. **后端只用标准库。** 新增第三方依赖前必须先论证必要性；目前第三方包只有三个：
   `lxn/walk` + `lxn/win`（备用原生窗口）、`skip2/go-qrcode`（二维码渲染）。
   `internal/server/` 这一层**零第三方依赖**。
2. **不引入 ffmpeg。** 封装是手写的，`internal/mux/` 中每一处字段顺序、基准偏移都注明了依据
   （如 ISO/IEC 14496-12 §8.8.8 对 `trun` 字段顺序的规定）。改这些前请先读懂注释。
3. **`elst` 重建必须丢掉空编辑项**（`internal/mux/mp4.go` 的 `patchElst`）。B 站很多源是
   「空编辑 + 真编辑」两段式，时长要写给**首条真编辑**（`media_time >= 0`）。
   早先无脑改首项，把整片时长写进了空编辑，于是整条轨道被推迟 212 秒、容器时长翻倍。
   `mp4_test.go` 里那组测试就是锁这个的。
4. **时长换算一律向上取整**（`scaleCeil`）。这类值会写进
   `tkhd` / `elst.segment_duration`，播放器拿它裁切媒体，向下取整会**丢掉末尾采样**。
5. **写接口必须带自定义头。** `internal/server` 只接受带 `X-BBDown-Native` 的 POST/PUT/DELETE，
   否则 403。这不是鉴权，而是 CSRF 防护 —— 跨站请求带自定义头会触发 CORS 预检，
   而服务端从不返回 CORS 头，浏览器会自己拦下。改路由时别把这层中间件绕过去。
6. **前端 `$("id")` 必须与 `index.html` 的 `id` 对应。** 对不上的引用是静默失效的，
   肉眼极难发现，所以 `server_test.go` 里有正则交叉校验（`TestFrontendIDsExistInHTML`）。
7. **凭证明文只允许出现在一处**：exe 同级的 `config.json`。不要写进日志、不要提交。
8. **剪贴板 / 目录选择框走 PowerShell，不用 syscall。** Win32 那条路必须把返回的 `uintptr`
   转成 `unsafe.Pointer`，而 `go vet` 的 `unsafeptr` 检查会拦（三种写法都试过，一律报
   misuse）。项目里因此**零 `unsafe`**，代价是慢半拍。
   脚本用 `-EncodedCommand`（base64/UTF-16LE）传参，中文与引号完全不经过命令行解析。
9. **备用原生窗口的 walk 尺寸规则比较反直觉**（`internal/gui/mainwin.go` 内有注释，仅在
   `-ui native` 下相关）：
   - `ComboBox` 非可编辑时不可压缩，**条目文案有多长控件就有多宽**，声明式 `MinSize` 对它无效；
   - `TextEdit`（非 compact）带 `GreedyVert`，会跟别的控件抢垂直空间，且 `MinSize` 无效；
   - `LineEdit` 相反，最小宽度只有 1 个字符，天生不会顶出窗口。

## 免责声明

本项目仅供个人学习，以及备份**已获得授权**的内容。请遵守哔哩哔哩用户协议与著作权法，
不要用于任何商业用途或大规模抓取。

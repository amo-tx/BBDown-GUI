# BBDown 图形界面 · 源码

这是图形界面的**源码与开发说明**。日常使用请直接双击上一层的
**`BBDown 图形界面.exe`**（已内置 Python 运行时，无需任何环境）。

给命令行工具 **BBDown** 套一层可视化操作台：粘贴链接 → 解析 → 勾选分P/画质 → 下载，
全程在**独立的桌面窗口**里完成（内嵌 WebView2），带实时日志与进度条，**不会跳转浏览器**。

## 快速开始（源码方式）

双击 **`启动 BBDown 图形界面.bat`** 即可。
脚本会自动找到 Python、启动本地服务并弹出**独立窗口**。

关闭窗口即退出程序。想用浏览器时，在界面右上角点「浏览器打开」即可（不会自动跳转）。

> 独立窗口依赖 `pywebview`：`pip install pywebview`。
> 若未安装，程序会**自动退回用系统浏览器打开**，功能不变。

## 前置条件

| 依赖 | 说明 |
| --- | --- |
| **BBDown.exe** | 放在上一层目录（即 `..\BBDown.exe`），本界面已自动指向它 |
| **ffmpeg** | **必需**。BBDown 即使只是解析也要求 ffmpeg。上一层 `..\tools\ffmpeg\bin\` 已内置；也可在界面里点「环境与账号 → 自动下载」重新获取 |
| **Python 3.8+** | 独立窗口需 `pywebview`；其余只用标准库 |

> 源码运行时，程序会自动按「上一层目录 → `tools\ffmpeg\bin\`」的顺序查找 ffmpeg，
> 因此源码模式与 exe 模式共用同一份 ffmpeg。

## 界面功能

- **视频地址**：支持 BV 号、av 号、ep/ss、完整链接；可多行输入批量下载。
  也能**直接粘贴带标题的分享文案**——地址识别由 `server.py:extract_targets()` 统一实现，
  前端只调 `/api/extract`，不做第二套正则（这样「提示的地址」与「实际下载的地址」永远一致）。
  「粘贴」按钮先试浏览器剪贴板 API，被拒时回退到 `/api/clipboard`（桌面窗口里后者更可靠）。
- **解析视频**：解析后显示标题、**UP主昵称**、**时长**、发布时间、可用流数量、分P列表（可勾选，带时长）。
  标题/封面/UP主/时长会用 B 站官方接口补充（接口不通时自动退回 BBDown 原始解析，不影响使用）。
- **下载设置**：解析接口（Web/TV/APP/国际版）、画质优先级、编码优先级、
  分P范围、混流语言、分P间隔；下载内容与附加开关（仅视频/仅音频/仅弹幕/
  仅字幕/仅封面、同时下弹幕、跳过字幕封面、跳过混流、交互式选清晰度、跳过重复）。
- **高级选项**：单P文件名模板、额外命令行参数、aria2c 加速。
- **环境与账号**：ffmpeg 路径与自动下载、保存目录（可选择/打开）、
  **四种登录方式**（见下节）。
- **主题切换**：右上角一键在 **普通 / 暗黑** 之间切换，偏好持久化在 `config.json`。
- **任务台**：实时日志滚动 + 进度条，可随时「停止」；下载所选分P。
  下载中额外显示**速度、已下载/总量、剩余时间、速度曲线、阶段步进（解析→视频→音频→混流）
  与已完成文件列表**，多分P会标出「第 n/N P」。
  发起扫码登录时，**二维码会直接显示在任务台顶部**，边看日志边扫码。

### 进度从哪来（`dlprogress.py`）

BBDown 的输出一旦被重定向（本程序用管道读它的日志），就**不打印任何百分比/速度/剩余时间**，
只有阶段标记。所以进度靠**监视磁盘分片增长**反推，纯标准库实现：

- 分片命名 `00000_<aid>.P<n>.<cid>.vclip`（视频）/ `.aclip`（音频），合并产物为 `*.mp4` / `*.m4a`；
- 单P落在 `<下载目录>/<aid>/`，多P落在 `<下载目录>/<视频标题>/` —— 因此是**递归扫描 + 正则识别**，
  不依赖目录名；
- 总量取解析日志里 `[视频] … [~286.14 MB]` 的预估值，实测单位为 **MiB**（误差约 0.1%）；
- `snapshot()` 对外给出：`active / stage / stageText / percent / speed / eta / page / pages /
  hasVideo / hasAudio / estimated / downloaded / total / outputs / done`，
  由 `server.py` 的监控线程每 0.4 秒取一次，挂到 `/api/task` 的 `prog` 字段。

踩过的坑（都已在代码里处理）：多分P解析后分母变大导致进度回退（用历史峰值保护）、
上次中断残留的分片被误计入（以任务开始时间为基线 + mtime 过滤）、
秒级完成的任务看不到进度（命中缓存为空时立刻全盘扫描）、
速度在 0～33 MB/s 间横跳（6 秒滑动窗口 + 指数平滑 + 停滞保持）。

## 登录方案

界面提供 **4 种登录方式**，都写进「环境与账号 → 账号登录」：

| 方式 | 入口 | 说明 |
| --- | --- | --- |
| **扫码登录（Web）** | 「扫码登录」页签 | 推荐。直接对接 B 站官方 Web 扫码接口，界面显示**真实二维码图片**与实时状态（未扫码 / 已扫码待确认 / 已失效 / 成功），失败自动换新码；登录成功后 cookie 自动写入 `BBDown.data` |
| **电视端登录** | 「电视端登录」页签 | 对接官方 TV 扫码接口，扫码后自动写入 `BBDownTV.data` 与 `access_token`。凭证有效期可达 180 天，且**不会把网页端登录挤下线**；TV/APP 接口的会员清晰度（4K / 8K / 杜比）需要它 |
| **手动 Cookie** | 「手动 Cookie」页签 | 从浏览器开发者工具复制整段粘贴，保存前会调账号接口**校验**，避免存进失效 Cookie |
| **access_token** | 「access_token」页签 | 手动填写（通常由电视端登录自动写入），保存后命令行自动附带 `--token` |

「退出登录」会清空配置里的凭证并删除 `BBDown.data` / `BBDownTV.data`。

### 为什么不再用 `BBDown login`

`BBDown login` / `logintv` **只在控制台用方块字符「画」二维码**，程序本身不返回登录状态。
实测该控制台二维码在本环境下会**退化成整整一片实心方块**（`login` 输出 65 行完全相同的
`█`，`logintv` 41 行），也就是说——**日志栏里根本不存在可扫描的图形**。此外旧实现还有两个问题：

1. `Task.push()` 会丢弃全空白行、并截断超长行，字符画即使正常也会被破坏；
2. 界面读的是工作目录里的 `qrcode.png`，那是**上一次登录残留的旧图**，且文件不存在时接口直接 404。

所以现在改为**自己实现扫码登录**：用官方接口拿 `qrcode_key` / `auth_code`，
本地生成真正的二维码图片（`qrcodegen.py`，纯标准库），再轮询真实登录状态。
不依赖 BBDown 的控制台输出，也不再依赖那个会过期的 `qrcode.png`。

> 旧的 `/qrcode` 接口仍然保留，仅作兼容。

## 主题

- 顶栏「☀ 普通 / ☾ 暗黑」一键切换，立即生效。
- 偏好写入浏览器 `localStorage`（保证刷新不闪白）并同步到 `config.json`；
  换台机器或换浏览器时，会沿用 `config.json` 里的主题。
- 独立窗口的初始底色也会跟随主题，避免暗色主题启动时闪一下白。
- 二维码区域在任何主题下都保持**白底黑码**，确保扫码识别率。

## 前端注意事项

- **日志框 `#console` 是唯一允许被压缩的弹性区**。任务台 `.console-card` 是 flex 纵向布局，
  而带 `overflow:hidden` 的 flex 子项其 `min-height:auto` 会退化为 `0` —— 窗口一变矮，
  阶段文案这类单行文本就会被挤成 0 高、文字整行消失。因此固定高度的区块都显式写了
  `flex:0 0 auto`，改任务台布局时别把它去掉。
- **主题色一律走 CSS 变量**，`:root` 与 `[data-theme="dark"]` 都要给值，不要在规则里硬编码颜色。
- 改完 `id` 记得与 `index.html` 交叉核对（前端用 `$("id")` 直接取元素，取不到会抛错，
  而启动链上的 `.catch(() => {})` 会把异常**静默吞掉**，表现为「界面一直停在等待任务」）。

## 目录结构

```
BBDown-GUI/                       # 本目录：源码
├─ server.py                      # 本地服务端（纯标准库）
│                                 #   含 extract_targets()：从分享文案里提取 B 站地址
├─ qrcodegen.py                   # 二维码生成器（纯标准库，PNG/SVG）
├─ bili_auth.py                   # 扫码登录（官方接口，Web + 电视端）
├─ dlprogress.py                  # 下载进度推算（从磁盘分片反推，纯标准库）
├─ web/                           # 前端页面
│  ├─ index.html
│  ├─ style.css                   # 含 light / dark 两套主题变量
│  └─ app.js
├─ assets/                        # 图标及其生成脚本
│  ├─ icon.ico
│  └─ make_icon.py
├─ tests/
│  ├─ url-extract-check.py        # 地址提取用例（分享文案 → 干净地址）
│  └─ render-check.py             # 前端渲染自检（进度面板 + 粘贴流程）
├─ 启动 BBDown 图形界面.bat        # 源码方式双击启动
└─ README.md

../                              # 上一层：运行时依赖与产物
├─ BBDown 图形界面.exe             # 打包好的成品（推荐日常使用）
├─ BBDown.exe
├─ tools/ffmpeg/bin/ffmpeg.exe
├─ downloads/                     # 默认下载目录
├─ BBDown.data                    # Web 登录后写入的 cookie（首次登录自动生成）
├─ BBDownTV.data                  # 电视端登录后写入的 access_token
└─ config.json                    # 配置（首次运行自动生成）
```

## 手动启动（可选）

```bash
python server.py                  # 默认：弹出独立窗口
python server.py --browser        # 改用系统浏览器打开
python server.py --no-browser     # 只启动后台服务，不开界面
python server.py --port 9000      # 指定端口
```

因为服务只监听 `127.0.0.1`，除本机外无法访问，安全上等同于本地程序。

## 接口一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/status` | 环境、版本、登录状态与配置 |
| GET | `/api/task?since=N` | 任务日志增量 + `prog` 进度快照（`percent/speed/eta/…`）+ `id`/`queueLeft` |
| POST | `/api/parse` · `/api/download` · `/api/stop` | 解析 / 下载 / 停止。地址字段用 `text`，可以是整段分享文案（服务端自行提取；旧字段 `url`/`urls` 仍兼容） |
| POST | `/api/extract` | 从任意文本提取 B 站地址，返回 `targets`/`targetCount`（前端「粘贴」与地址提示用） |
| GET | `/api/clipboard` | 读取系统剪贴板文本（浏览器剪贴板 API 不可用时的回退） |
| POST | `/api/login/start` | 开始扫码登录，`mode` = `web` 或 `tv` |
| GET | `/api/login/status?mode=` | 扫码状态（含剩余秒数与账号信息） |
| GET | `/api/login/qrcode.png?mode=` | 当前登录二维码图片（PNG） |
| POST | `/api/login/cancel` | 取消扫码 |
| POST | `/api/login/cookie` | 校验并保存手动 Cookie |
| POST | `/api/login/token` | 保存 access_token |
| POST | `/api/login/logout` | 退出登录 |
| POST | `/api/config` | 保存配置（含 `theme`） |

## 自检（开发用）

```bash
python qrcodegen.py                     # 打印示例二维码（终端字符画）
python bili_auth.py                     # 生成一次真实扫码会话并打印状态变化
python tests\url-extract-check.py       # 地址提取用例（分享文案 → 干净地址）
python tests\render-check.py            # 前端渲染自检（进度面板 + 粘贴流程，见下）
```

`tests\url-extract-check.py` 覆盖 `extract_targets()` 的 30 多个用例：真实分享文案、
中文/全角标点与括号引号包裹、Markdown 链接、一段话里多个地址（保序 + 去重）、
BV/av/ep/ss 裸号、b23.tv 短链、非 B 站链接应被忽略等，并检查归一化是否幂等。
退出码非 0 表示有用例未通过。

`tests\render-check.py` 用**打桩数据**驱动真实前端（真 `index.html` + 真 `app.js` +
真 `style.css`），稳定复现「多分P传输中」画面，截图到 `_shots\`，并断言速度 / 已下载 /
剩余时间 / 阶段步进 / 进度条 / 产物列表都真的渲染到了 DOM 上；同时检查普通、暗黑两种主题，
以及 1460×620 的矮窗口（防止 flex 压缩把单行文案挤没）。
它还会真的点一次「粘贴」按钮，验证三条路径（浏览器剪贴板 / 服务端回退 / 多地址）都能
把文案变成输入框里的干净地址——`/api/extract`、`/api/clipboard` 在这条验证里**不打死桩**，
直连真实服务端，所以提取规则与前端接线是一次性打通的。

它会自动在 8813 端口拉起 `server.py`（已在跑就复用），跑完自动收工并清理临时文件，
退出码非 0 表示有断言未通过。之所以要打桩而不是真下载：本机带宽下几十 MB 的分P
一两秒就跑完，无头浏览器启动却要 2~3 秒，几乎必然错过传输阶段。

## 重新打包为 exe

```bash
pip install pyinstaller pywebview pythonnet
cd ..            # 到上一层目录（BBDown 图形界面.exe 所在目录）
pyinstaller --noconfirm --clean --onefile --windowed ^
  --name BBDownGUI ^
  --icon BBDown-GUI\assets\icon.ico ^
  --add-data "BBDown-GUI\web;web" ^
  --add-data "BBDown-GUI\assets\icon.ico;assets" ^
  --paths BBDown-GUI ^
  --collect-all webview --collect-all clr_loader --collect-all pythonnet ^
  --hidden-import webview.platforms.winforms ^
  --hidden-import webview.platforms.edgechromium ^
  --hidden-import clr ^
  --hidden-import qrcodegen --hidden-import bili_auth ^
  --hidden-import dlprogress ^
  BBDown-GUI\server.py
```

生成 `dist\BBDownGUI.exe`，改名为 `BBDown 图形界面.exe` 并放到上一层目录即可。

> 必须用 `--windowed`（不要 `--console`），否则会多一个命令行黑窗；
> 也不要加 `--exclude-module distutils`，会与 PyInstaller 内置 hook 冲突报错。
> 环境里若装了 `enum34` 这类被标准库取代的包，PyInstaller 会直接报错，先卸载。
>
> **打包后务必用 `BBDownGUI.exe --no-browser --port 8814` 起一次**，
> 再 `curl` 一下 `/api/status` 与 `/api/task`，确认 BBDown/ffmpeg 路径与 `prog` 字段都正常。
> （打包版会把 `config.json` 写到 exe 同级目录，即 `dist\`。）
>
> `server.py` 同时兼容「源码运行」与「打包运行」两种路径：
> 打包后 `web/` 与 `assets/` 从前端资源包（`sys._MEIPASS`）读取，
> 而 `config.json`、`tools/`、`downloads/` 始终落在 **exe 同级目录**，不会写进临时解压目录。

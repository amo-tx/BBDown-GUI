# BBDown 图形界面

给命令行工具 **BBDown** 套一层可视化操作台：粘贴链接 → 解析 → 勾选分P/画质 → 下载，
全程在**独立的桌面窗口**里完成，带实时日志与进度条，**不会跳转浏览器**。

## 快速开始

**双击 `BBDown 图形界面.exe`** 即可。

程序会直接弹出一个独立的窗口（内嵌 WebView2），**没有命令行黑窗、也不会打开浏览器**。
关闭窗口即退出程序。

> 已内置 Python 运行时，**无需安装 Python，也无需 pip 安装任何依赖**。

重复双击不会开出多个实例：若窗口已在运行，会自动把**已有窗口切到前台**。

## 前置条件

| 依赖 | 说明 |
| --- | --- |
| **BBDown.exe** | 与本程序放在**同一目录**（已随包提供） |
| **ffmpeg** | **必需**。BBDown 即使只是解析也要求 ffmpeg。已随包放在 `tools\ffmpeg\bin\` |
| WebView2 运行时 | Windows 10/11 已随 Edge 内置，通常无需额外安装 |

## 目录结构

```
BBDown_1.6.3_20240814_win-x64\
├─ BBDown 图形界面.exe            ← 双击这个（独立窗口）
├─ BBDown.exe                     ← 下载核心（必需）
├─ tools\ffmpeg\bin\ffmpeg.exe    ← 混流工具（必需）
├─ downloads\                     ← 默认下载目录（首次下载时自动创建）
├─ config.json                    ← 你的配置（首次运行自动生成）
├─ BBDown.data                    ← Web 扫码登录后写入的 cookie
├─ BBDownTV.data                  ← 电视端扫码登录后写入的 access_token
└─ BBDown-GUI\                    ← 源码（想改界面/重新打包时用）
   ├─ server.py                   ← 后端 + 窗口宿主
   ├─ qrcodegen.py                ← 二维码生成器（纯标准库）
   ├─ bili_auth.py                ← 扫码登录（官方接口，Web + 电视端）
   ├─ dlprogress.py               ← 下载进度推算（从磁盘分片反推，纯标准库）
   ├─ web\{index.html,style.css,app.js}
   ├─ assets\{icon.ico,make_icon.py}   ← 含窗口/任务栏图标
   ├─ tests\{url-extract-check,render-check}.py   ← 开发自检
   ├─ 启动 BBDown 图形界面.bat     ← 源码方式启动（需装 Python）
   └─ README.md
```

> 界面自身的缓存（WebView2 用户数据）写在 `%LOCALAPPDATA%\BBDownGUI\webview-data`，
> 不会堆在本目录里。

## 界面功能

- **视频地址**：支持 BV 号、av 号、ep/ss、完整链接；可多行输入批量下载。
  **可直接粘贴带标题的分享文案**（如 `【标题】 https://www.bilibili.com/video/BV.../?share_source=...`），
  程序会自动从中取出地址、去掉多余文字；一段话里有多个地址时会依次下载；
  旁边的「粘贴」按钮可一键读剪贴板（桌面窗口 / 浏览器都可用）。
- **解析视频**：显示标题、**UP主昵称**、**时长**、发布时间、可用流数量、分P列表（可勾选，带时长）。
- **下载设置**：解析接口（Web/TV/APP/国际版）、画质优先级、编码优先级、分P范围、混流语言、分P间隔；
  仅视频/仅音频/仅弹幕/仅字幕/仅封面、同时下弹幕、跳过字幕封面、跳过混流、交互式选清晰度。
- **高级选项**：单P文件名模板、额外命令行参数、aria2c 加速、下载链路兼容性。
- **环境与账号**：ffmpeg 路径与自动下载、保存目录（可选择/打开）、
  **四种登录方式**（网页扫码 / 电视端扫码 / 手动 Cookie / access_token）。
- **主题切换**：右上角一键切换 **普通 / 暗黑**，偏好会被记住。
- **任务台**：实时日志滚动 + 进度条，可随时「停止」；可「下载所选分P」。
  下载时额外显示**速度、已下载/总量、剩余时间、速度曲线、阶段步进（解析→视频→音频→混流）、
  已完成文件列表**；分P任务会标出「第 n/N P」。
  发起扫码登录时，**二维码直接显示在任务台顶部**，旁边就是日志。

## 下载进度是怎么算出来的

这一点值得单独说明，因为 **BBDown 本身不报告进度**。

实测发现：只要把 BBDown 的输出重定向到管道（本程序就是这么做的），它就**完全不打印百分比、
速度和剩余时间**，只输出「开始下载P1视频…」「开始下载P1音频…」「合并音视频分片…」这类阶段标记。
所以进度只能从**磁盘反推**：

| 数据 | 来源 |
| --- | --- |
| 总量 | 解析时 `已选择的流: [视频] … [~286.14 MB]` 里的预估值（实测单位是 MiB，误差约 0.1%） |
| 已下载 | 监视工作目录下 `.vclip` / `.aclip` 分片的字节数增长 |
| 速度 | 6 秒滑动窗口差分 + 指数平滑，窗口内没有新增时保留上一次速度 |
| 剩余时间 | 按整个任务（含未开始的分P）折算，避免多分P时严重虚报 |
| 阶段 | 解析 BBDown 的阶段标记行 |

几个已处理的坑：

- **进度只增不减**：多分P解析后分母会变大，用历史峰值保护，避免出现 3.00% → 1.50% 的回退。
- **旧残留不算数**：上一次中断留下的分片会被排除（以任务开始时间为基线，更早的文件不计入）。
- **快下载也能看见**：秒下完的任务会绕过目录缓存立刻全盘扫描，不会一直卡在 0%。
- **产物列表干净**：只列最终成品，临时文件（`{aid}.tmp`、`.vclip` 分片）和封面不入列。

实现集中在 `BBDown-GUI\dlprogress.py`（纯标准库），后端每 0.4 秒取一次快照推给前端。

## 扫码登录说明

登录二维码由程序**自己生成并显示为真正的图片**（不再是 BBDown 在控制台用方块字符
「画」的那种）。发起登录后：

1. 任务台顶部出现二维码面板，同时「环境与账号 → 账号登录」里也会显示；
2. 手机哔哩哔哩 App 扫码 → 手机上点确认；
3. 界面实时显示「等待扫码 / 已扫码待确认 / 登录成功」，成功后凭证自动写入
   `BBDown.data`，之后无需再次登录。

- **网页扫码**：推荐日常使用，获取网页端 cookie。
- **电视端扫码**：获取 `access_token`（写入 `BBDownTV.data`），有效期可达 180 天，
  且不会把浏览器里的登录挤下线；下载 4K / 8K / 杜比等会员清晰度需要它。

二维码 180 秒失效后会自动重新申请，无需手动干预。

### 为什么旧版看不到二维码

旧实现依赖 `BBDown login` 在控制台打印的字符画二维码，而实测该输出在本环境下会**退化成
一整片实心方块**（`login` 65 行、`logintv` 41 行，都是完全相同的 `█`），日志栏里根本不存在
可扫描的图形；此外旧的 `qrcode.png` 是上一次登录的残留文件，前端读到的也是旧图。
现在改为直接对接 B 站官方登录接口、本地生成二维码图片并轮询真实状态，已彻底绕开上述问题。

## 两个实用细节

**403 Forbidden 会自动处理。** BBDown 默认会强制替换下载主机与协议（`--force-http`、
`--force-replace-host`），在部分网络下会被 CDN 拒绝。本程序检测到 403 后会**自动关闭这两项并重试**，
用户无感。也可在「高级选项 → 下载链路兼容性」里直接勾选「兼容模式」避免首次失败。

**建议先登录。** 该网络环境下 B 站风控较敏感，无 Cookie 时字幕接口会返回 418。
点「环境与账号 → 账号登录 → 扫码登录」，二维码会显示在任务台与登录面板里。

## 界面运行方式

界面本身是一个本地网页，但**由程序内嵌在一个原生窗口里显示**（Windows WebView2 内核），
所以体验上就是一个普通桌面软件：

- 双击 exe → 弹出独立窗口，没有控制台黑窗，也不会打开系统浏览器。
- 标题栏右上角有「**浏览器打开**」（想用浏览器时手动点，不会自动跳）和「**退出**」。
- 窗口内可正常使用：粘贴、扫码登录、右键、复制日志、Ctrl+滚轮缩放。

如果机器上缺少 WebView2（极少见），程序会**自动退回用系统浏览器打开**，功能完全一致。

## 源码方式启动（可选）

源码模式下若已安装 Python 3.8+ 与 `pywebview`，同样可以开出独立窗口：

```bash
pip install pywebview
cd BBDown-GUI
python server.py                  # 默认：独立窗口
python server.py --browser        # 改用系统浏览器打开
python server.py --no-browser     # 只启动后台服务，不开界面
python server.py --port 9000      # 指定端口
```

或双击 `BBDown-GUI\启动 BBDown 图形界面.bat`。

## 重新打包 exe

```bash
pip install pyinstaller pywebview pythonnet
pyinstaller --noconfirm --clean --onefile --windowed \
  --name BBDownGUI \
  --icon BBDown-GUI\assets\icon.ico \
  --add-data "BBDown-GUI\web;web" \
  --add-data "BBDown-GUI\assets\icon.ico;assets" \
  --paths BBDown-GUI \
  --collect-all webview --collect-all clr_loader --collect-all pythonnet \
  --hidden-import webview.platforms.winforms \
  --hidden-import webview.platforms.edgechromium \
  --hidden-import clr \
  --hidden-import qrcodegen --hidden-import bili_auth \
  --hidden-import dlprogress \
  BBDown-GUI\server.py
```

> 注意用 `--windowed`（不要 `--console`），否则会多出一个命令行黑窗。
> 也**不要**加 `--exclude-module distutils`，会与 PyInstaller 内置 hook 冲突报错。
> 另外若环境里装了 `enum34` 这类已被标准库取代的包，PyInstaller 会直接报错，先卸载即可。
>
> 打包后**务必**用 `BBDownGUI.exe --no-browser --port 8814` 起一次，`curl` 一下
> `/api/status` 与 `/api/task`，确认 BBDown/ffmpeg 路径和 `prog` 字段都正常，
> 再替换根目录的 `BBDown 图形界面.exe`。

生成 `dist\BBDownGUI.exe`，改名为 `BBDown 图形界面.exe` 放到本目录即可。

## 安全说明

服务只监听 `127.0.0.1`（本机回环），外部无法访问，安全上等同于本地程序。

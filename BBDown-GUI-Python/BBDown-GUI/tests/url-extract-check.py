# -*- coding: utf-8 -*-
"""地址提取自检：server.py:extract_targets 的回归用例。

为什么要单测这个：它决定「用户粘贴什么」能不能变成「BBDown 认识的地址」。
B 站 App / 网页复制出来的是一整段分享文案（标题 + 链接 + 追踪参数），
BBDown 只认干净地址，整段喂进去只会停在「获取aid... 输入有误」。

用例覆盖：
  * 真实分享文案（含标题、中英文混合、emoji、?share_source&vd_source）
  * 链接前后粘着中文 / 全角标点 / 括号 / 引号 / Markdown 语法
  * 同一条文本里的多个链接（保序）与重复项（去重）
  * 裸号：BV / 小写 bv / av / ep / ss
  * 非 B 站链接应被忽略（否则会被当成地址丢给 BBDown）

跑法： python BBDown-GUI/tests/url-extract-check.py
退出码非 0 表示有用例未通过。
"""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
GUI = os.path.dirname(HERE)
sys.path.insert(0, GUI)

import server  # noqa: E402  （导入即读 config.json，不会起服务）

BV = "BV12eHa6RE59"
BV2 = "BV16VHL6NEQe"
BV3 = "BV1GJ411x7h7"
SHARE_URL = ("https://www.bilibili.com/video/" + BV +
             "/?share_source=copy_web&vd_source=948b033cbf8aaa1bf41502231246ba13")

# (说明, 输入文本, 期望结果)
CASES = [
    # ---- 真实分享文案 ---------------------------------------------------- #
    ("App 分享文案（本轮要解决的场景）",
     "【Hsin let's Groove!】 " + SHARE_URL, [SHARE_URL]),
    ("分享文案带 emoji",
     "【标题🎵】 " + SHARE_URL + " 🎶", [SHARE_URL]),
    ("文案里的链接后跟中文",
     "看这个 https://www.bilibili.com/video/" + BV + "/ 很有意思，快来看！",
     ["https://www.bilibili.com/video/" + BV + "/"]),
    ("链接后紧跟全角逗号",
     "【标题】https://www.bilibili.com/video/" + BV + "/，复制此链接打开",
     ["https://www.bilibili.com/video/" + BV + "/"]),
    ("句末句号",
     "链接：https://b23.tv/" + BV + ".", ["https://b23.tv/" + BV]),

    # ---- 包裹符号 -------------------------------------------------------- #
    ("中文圆括号包裹", "（https://www.bilibili.com/video/" + BV + "/）",
     ["https://www.bilibili.com/video/" + BV + "/"]),
    ("英文双引号包裹", '"https://www.bilibili.com/video/' + BV + '/"',
     ["https://www.bilibili.com/video/" + BV + "/"]),
    ("Markdown 链接语法", "[标题](https://www.bilibili.com/video/" + BV + "/)",
     ["https://www.bilibili.com/video/" + BV + "/"]),
    ("英文句子中的链接",
     "Watch this: https://www.bilibili.com/video/" + BV + "/. Enjoy!",
     ["https://www.bilibili.com/video/" + BV + "/"]),

    # ---- 裸号 ------------------------------------------------------------ #
    ("裸 BV 号", BV, [BV]),
    ("小写 bv 前缀（BBDown 接受，原样传递）", "bv12eHa6RE59", ["bv12eHa6RE59"]),
    ("裸 av 号（长号也要认）", "av117375886169591", ["av117375886169591"]),
    ("裸 ep 号", "ep1234", ["ep1234"]),
    ("裸 ss 号", "ss4321", ["ss4321"]),

    # ---- 链接形态 -------------------------------------------------------- #
    ("b23.tv 短链", "https://b23.tv/" + BV, ["https://b23.tv/" + BV]),
    ("移动端链接", "https://m.bilibili.com/video/" + BV,
     ["https://m.bilibili.com/video/" + BV]),
    ("缺协议但有 www.", "www.bilibili.com/video/" + BV + "/",
     ["https://www.bilibili.com/video/" + BV + "/"]),
    ("缺协议且无 www.", "bilibili.com/video/" + BV,
     ["https://bilibili.com/video/" + BV]),
    ("保留 p 参数（BBDown 会据此选集）",
     "https://www.bilibili.com/video/" + BV2 + "/?p=2",
     ["https://www.bilibili.com/video/" + BV2 + "/?p=2"]),

    # ---- 多个链接 -------------------------------------------------------- #
    ("一行一个（多行输入）",
     "https://www.bilibili.com/video/" + BV + "/\nhttps://b23.tv/" + BV2,
     ["https://www.bilibili.com/video/" + BV + "/", "https://b23.tv/" + BV2]),
    ("一段话里的多个链接 + 顺序保持",
     "【1】https://www.bilibili.com/video/" + BV + "/?share_source=copy_web\n"
     "【2】" + BV2 + " 还有 https://www.bilibili.com/video/" + BV3,
     ["https://www.bilibili.com/video/" + BV + "/?share_source=copy_web",
      BV2, "https://www.bilibili.com/video/" + BV3]),
    ("裸号与空行混杂",
     "多行\n" + BV3 + "\n\n   " + BV + "  ", [BV3, BV]),

    # ---- 去重 ------------------------------------------------------------ #
    ("同一视频的 BV 与完整链接算一个",
     BV + " https://www.bilibili.com/video/" + BV + "/", [BV]),
    ("短链与完整链接算一个（BV 相同）",
     "https://www.bilibili.com/video/" + BV + "/ https://b23.tv/" + BV,
     ["https://www.bilibili.com/video/" + BV + "/"]),
    ("重复的裸号只留一个", BV + " " + BV + " " + BV, [BV]),

    # ---- 应当忽略 -------------------------------------------------------- #
    ("非 B 站链接被忽略（但同行的 BV 保留）",
     "https://www.youtube.com/watch?v=abc123 和 " + BV, [BV]),
    ("纯非 B 站链接", "https://www.google.com/search?q=bilibili", []),
    ("没有地址的普通文本", "你好，这是一段普通的话", []),
    ("空文本", "", []),
    ("只有空白", "   \n\t ", []),
    # 这两个是「看着像但其实不是」的反例：没有 av/ep/ss 前缀的纯数字不该被当成地址
    ("纯数字不是地址", "1234567890", []),
    ("普通英文单词不是地址", "have a nice day, 2026", []),
]


def check_normalize(targets):
    """归一化必须幂等：把结果再喂回去应得到同样的列表。"""
    return server.extract_targets("\n".join(targets))


def main():
    fails, total = [], 0
    for name, text, want in CASES:
        total += 1
        got = server.extract_targets(text)
        if got != want:
            fails.append("%s\n      输入: %r\n      期望: %r\n      实际: %r"
                         % (name, text, want, got))
            continue
        again = check_normalize(got)
        if again != got:
            fails.append("%s —— 归一化不幂等：%r → %r" % (name, got, again))

    # 旧接口兼容：前端历史上传的是 url / urls 字段
    total += 1
    legacy = server.extract_targets(
        server.address_text({"urls": [BV, BV2], "url": "【x】" + BV}))
    if legacy != [BV, BV2]:
        fails.append("旧字段 url/urls 兼容失败：%r" % (legacy,))

    print("=" * 52)
    print("地址提取自检：%d 个用例" % total)
    print("=" * 52)
    for name, text, want in CASES:
        got = server.extract_targets(text)
        ok = got == want
        print("  %s %s" % ("OK  " if ok else "FAIL", name))
        if not ok:
            print("       输入: %r" % (text,))
            print("       期望: %r" % (want,))
            print("       实际: %r" % (got,))
    print("-" * 52)
    if fails:
        print("结果：%d 项未通过" % len(fails))
        for f in fails:
            print("  x " + f)
        return 1
    print("结果：全部通过")
    return 0


sys.exit(main())

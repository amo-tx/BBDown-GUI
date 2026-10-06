package gui

import (
	"github.com/lxn/walk"
)

// Palette 里每一项都对应老网页 style.css 的一个 CSS 变量。
//
// 之所以要能一一对上，是因为这次的界面目标就是「把老网页那套外观画进
// 原生窗口」—— 配色一旦自己发挥，两边就会漂移，改一处忘一处。
type Palette struct {
	Dark bool

	Bg       walk.Color // --bg        页面底色
	Surface  walk.Color // --surface   卡片底
	Surface2 walk.Color // --surface-2 次级底（输入框、条纹）
	Surface3 walk.Color // --surface-3 三级底（悬停、标签底）
	Line     walk.Color // --line      分隔线
	LineStr  walk.Color // --line-strong 强调分隔线 / 输入框边框

	Text  walk.Color // --text   主文字
	Text2 walk.Color // --text-2 次要文字
	Text3 walk.Color // --text-3 提示文字

	Accent     walk.Color // --accent      B 站粉
	Accent2    walk.Color // --accent-2    粉色深一档（按下 / 渐变末端）
	AccentSoft walk.Color // --accent-soft 粉色浅底（选中标签底）

	Blue  walk.Color
	Green walk.Color
	Amber walk.Color
	Red   walk.Color

	OkBg, OkFg, OkLine    walk.Color
	BadBg, BadFg, BadLine walk.Color

	// 日志控制台在**两种主题下都是深色** —— 老网页就是这么做的：
	// 深色底让日志一眼能跟正文区分开，长时间盯着也更省眼。
	ConsoleBg  walk.Color
	ConsoleFg  walk.Color
	ConsoleDim walk.Color
}

// hex 把 0xRRGGBB 写成 walk 的 COLORREF（0x00BBGGRR 次序）。
//
// 直接写 walk.RGB(r,g,b) 也行，但那样每行要拆三个数字，改配色时非常容易
// 看错位；用十六进制可以原样抄 style.css 里的值。
func hex(v uint32) walk.Color {
	return walk.RGB(byte(v>>16), byte(v>>8), byte(v))
}

func lightPalette() Palette {
	return Palette{
		Bg:       hex(0xf4f6fb),
		Surface:  hex(0xffffff),
		Surface2: hex(0xf8fafc),
		Surface3: hex(0xeef2f9),
		Line:     hex(0xe6eaf2),
		LineStr:  hex(0xd7deea),

		Text:  hex(0x1f2733),
		Text2: hex(0x5b6676),
		Text3: hex(0x8b96a6),

		Accent:     hex(0xfb7299),
		Accent2:    hex(0xf43f7f),
		AccentSoft: hex(0xfff0f5),

		Blue:  hex(0x3b82f6),
		Green: hex(0x10b981),
		Amber: hex(0xf59e0b),
		Red:   hex(0xef4444),

		OkBg: hex(0xecfdf5), OkFg: hex(0x047857), OkLine: hex(0xa7f3d0),
		BadBg: hex(0xfef2f2), BadFg: hex(0xb91c1c), BadLine: hex(0xfecaca),

		ConsoleBg:  hex(0x101728),
		ConsoleFg:  hex(0xcdd7ea),
		ConsoleDim: hex(0x7d8aa5),
	}
}

func darkPalette() Palette {
	return Palette{
		Dark:     true,
		Bg:       hex(0x0b0f18),
		Surface:  hex(0x151b28),
		Surface2: hex(0x1b2231),
		Surface3: hex(0x222b3d),
		Line:     hex(0x252d3e),
		LineStr:  hex(0x35415a),

		Text:  hex(0xe7ecf6),
		Text2: hex(0xa9b5c9),
		Text3: hex(0x77839b),

		Accent:     hex(0xfb7299),
		Accent2:    hex(0xff9bb8),
		AccentSoft: hex(0x3a2432),

		Blue:  hex(0x5b9bff),
		Green: hex(0x34d399),
		Amber: hex(0xfbbf24),
		Red:   hex(0xf87171),

		OkBg: hex(0x0d2a22), OkFg: hex(0x6ee7b7), OkLine: hex(0x175544),
		BadBg: hex(0x2a1414), BadFg: hex(0xfca5a5), BadLine: hex(0x5c2626),

		ConsoleBg:  hex(0x0b0f18),
		ConsoleFg:  hex(0xcdd7ea),
		ConsoleDim: hex(0x6d7a94),
	}
}

func paletteFor(dark bool) Palette {
	if dark {
		return darkPalette()
	}
	return lightPalette()
}

// ---------------------------------------------------------------------------
// 字体

// fonts 是整套界面用到的字体。创建一次、全程复用。
//
// 每个 walk.Font 内部持有一个 GDI 字体句柄，按窗口 DPI 缓存；在 paint 里
// 反复 NewFont 会一路泄漏 GDI 对象（Windows 每进程上限 10000），
// 所以绝不能在绘制路径上构造字体。
type fonts struct {
	Title  *walk.Font // 窗口标题
	Head   *walk.Font // 卡片标题
	Body   *walk.Font // 正文
	Bold   *walk.Font // 强调正文
	Small  *walk.Font // 按钮、次级文字
	SmallB *walk.Font // 按钮加粗
	Tiny   *walk.Font // 提示
	Mono   *walk.Font // 日志控制台
}

// cnFonts 按优先级挑一个存在的中文字体族。
//
// 「Microsoft YaHei UI」是 Win8+ 的界面字体，字重与字距都比老的
// 「Microsoft YaHei」更适合小字号；老系统上退回后者。
var cnFonts = []string{"Microsoft YaHei UI", "Microsoft YaHei", "SimHei"}

func pickFamily(candidates []string, fallback string) string {
	for _, f := range candidates {
		if _, err := walk.NewFont(f, 9, 0); err == nil {
			return f
		}
	}
	return fallback
}

func newFonts() (*fonts, error) {
	// walk.NewFont 对不存在的字体族不会报错（GDI 自己会替换），所以这里
	// 不必探测，直接用第一个候选。
	family := cnFonts[0]
	mono := "Consolas"

	mk := func(size int, style walk.FontStyle) (*walk.Font, error) {
		return walk.NewFont(family, size, style)
	}

	f := &fonts{}
	var err error
	if f.Title, err = mk(14, walk.FontBold); err != nil {
		return nil, err
	}
	if f.Head, err = mk(11, walk.FontBold); err != nil {
		return nil, err
	}
	if f.Body, err = mk(10, 0); err != nil {
		return nil, err
	}
	if f.Bold, err = mk(10, walk.FontBold); err != nil {
		return nil, err
	}
	if f.Small, err = mk(9, 0); err != nil {
		return nil, err
	}
	if f.SmallB, err = mk(9, walk.FontBold); err != nil {
		return nil, err
	}
	if f.Tiny, err = mk(8, 0); err != nil {
		return nil, err
	}
	if f.Mono, err = walk.NewFont(mono, 9, 0); err != nil {
		return nil, err
	}
	return f, nil
}

package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/walk/declarative"

	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/cookie"
)

// 账号卡上五个入口的编号。
const (
	loginTabQR = iota
	loginTabTV
	loginTabBrowser
	loginTabFile
	loginTabPassword
)

// openLoginDialog 按入口打开对应的登录窗口。
//
// 五种方式各开各的窗口，而不是塞进一个多标签页的大对话框：除了扫码那一支，
// 其余几支要么只是「从列表里挑一个」，要么只有两个输入框，每个窗口都简单到
// 不需要额外解释。合成一个反而要额外处理「切标签时正在跑的网络请求怎么办」。
func (w *Win) openLoginDialog(tab int) {
	switch tab {
	case loginTabTV:
		w.loginByQR(bilibili.LoginTV)
	case loginTabBrowser:
		w.loginFromBrowser()
	case loginTabFile:
		w.loginFromFile()
	case loginTabPassword:
		w.loginByPassword()
	default:
		w.loginByQR(bilibili.LoginWeb)
	}
}

// ---------------------------------------------------------------------------
// 扫码（网页端 / 电视端）

// qrLogin 是扫码窗口的控制器。
//
// 它自己不碰网络与状态机 —— 状态全在 bilibili.LoginSession 里，
// 这里只负责「每 400ms 取一次快照，变了就重画」。
type qrLogin struct {
	dlg    *walk.Dialog
	img    *walk.ImageView
	status *walk.Label
	remain *walk.Label
	urlLB  *walk.LineEdit

	// 退出时回填的结果
	result *bilibili.LoginResult
}

// loginByQR 打开扫码窗口。mode 决定走网页端还是电视端接口。
func (w *Win) loginByQR(mode bilibili.LoginMode) {
	title := "扫码登录 · 网页端"
	tip := "用「哔哩哔哩」手机 App 扫码。网页端凭证约 30 天有效。"
	if mode == bilibili.LoginTV {
		title = "扫码登录 · 电视端"
		tip = "用「哔哩哔哩」手机 App 扫码。电视端凭证约 180 天有效，适合长期挂机。"
	}

	d := &qrLogin{}
	dlgDef := declarative.Dialog{
		AssignTo:  &d.dlg,
		Title:     title,
		FixedSize: true,
		Size:      declarative.Size{Width: 392, Height: 566},
		Layout:    declarative.VBox{Spacing: 7},
		Children: []declarative.Widget{
			declarative.Label{Text: tip},
			declarative.Composite{
				Layout: declarative.VBox{MarginsZero: true},
				Children: []declarative.Widget{
					// 二维码区域固定白底 —— 二维码必须白底黑码，
					// 深色主题下也不能反色，否则扫码率会掉。
					declarative.ImageView{
						AssignTo:   &d.img,
						MinSize:    declarative.Size{Width: 320, Height: 320},
						MaxSize:    declarative.Size{Width: 320, Height: 320},
						Mode:       declarative.ImageViewModeZoom,
						Background: declarative.SolidColorBrush{Color: walk.RGB(255, 255, 255)},
					},
				},
			},
			declarative.Label{AssignTo: &d.status, Text: "正在申请二维码…", Alignment: declarative.AlignHCenterVCenter},
			declarative.Label{AssignTo: &d.remain, Text: "", Alignment: declarative.AlignHCenterVCenter},
			declarative.LineEdit{
				AssignTo:  &d.urlLB,
				ReadOnly:  true,
				CueBanner: "二维码里的链接（一般不用管）",
			},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 6},
				Children: []declarative.Widget{
					declarative.PushButton{
						Text:    "重新获取",
						MinSize: declarative.Size{Width: 96},
						OnClicked: func() {
							w.login.Cancel()
							d.restart(w, mode)
						},
					},
					declarative.HSpacer{},
					declarative.PushButton{
						Text:      "取消",
						MinSize:   declarative.Size{Width: 80},
						OnClicked: func() { w.login.Cancel(); d.dlg.Cancel() },
					},
				},
			},
		},
	}

	// 先建控件、再模态显示，首轮扫码等消息循环起来之后再发起。
	//
	// 两个坑叠在一起：
	//  1. restart（发起扫码 + 启动 pump）原来只有「重新获取」按钮才会调，
	//     打开窗口时从没发起过任何请求，二维码永远是空的；
	//  2. 不能在 dlg.Run() 之前直接调 restart —— pump 靠 Synchronize 把状态
	//     刷回界面，而 Synchronize 排队等的是**对话框自己那条消息循环**去消费；
	//     循环还没跑时投递的回调送不进去（按钮能正常工作，正是因为它本身就在
	//     循环里被派发）。所以这里用 AfterFunc 把首轮扫码推到循环启动之后。
	if err := dlgDef.Create(w.mw); err != nil {
		w.log("登录窗口打开失败：%v", err)
		return
	}
	time.AfterFunc(120*time.Millisecond, func() {
		d.dlg.Synchronize(func() { d.restart(w, mode) })
	})
	d.dlg.Run()

	// 窗口关掉后一定要停掉后台轮询 —— 否则它会一直请求到二维码失效为止。
	w.login.Cancel()

	if d.result != nil {
		w.applyLogin(d.result)
	}
}

// restart 重新开始一轮扫码。
func (d *qrLogin) restart(w *Win, mode bilibili.LoginMode) {
	ok, msg := w.login.Start(mode, nil)
	if !ok {
		d.status.SetText(msg)
		return
	}
	d.status.SetText("正在申请二维码…")
	d.remain.SetText("")
	d.urlLB.SetText("")
	d.result = nil
	go d.pump(w)
}

// pump 轮询快照，把变化画到界面上；成功后关窗。
func (d *qrLogin) pump(w *Win) {
	lastSeq, lastImg := -1, -1
	for {
		if d.dlg == nil {
			return
		}
		s := w.login.Snapshot()
		if s.Seq != lastSeq {
			lastSeq = s.Seq
			snap := s
			d.dlg.Synchronize(func() {
				if d.status != nil {
					d.status.SetText(snap.Message)
				}
				if snap.URL != "" && d.urlLB != nil {
					d.urlLB.SetText(snap.URL)
				}
			})
		}
		if s.Remain > 0 {
			r := s.Remain
			d.dlg.Synchronize(func() {
				if d.remain != nil {
					d.remain.SetText(fmt.Sprintf("%d 秒后失效", r))
				}
			})
		}

		// 二维码换了才重建位图，别每轮都造一张 —— 每张 320×320 的位图
		// 都是一个 GDI 对象，400ms 造一张，一分钟就是 150 个。
		if s.Seq != lastImg {
			if img := w.login.QRImage(); img != nil {
				lastImg = s.Seq
				bm, err := walk.NewBitmapFromImage(img)
				if err != nil {
					d.dlg.Synchronize(func() { d.status.SetText("二维码渲染失败：" + err.Error()) })
				} else {
					d.dlg.Synchronize(func() {
						if d.img != nil {
							_ = d.img.SetImage(bm)
						}
					})
				}
			}
		}

		switch s.State {
		case bilibili.StateConfirmed:
			if s.Result != nil {
				d.result = s.Result
			}
			d.dlg.Synchronize(func() { d.dlg.Accept() })
			return
		case bilibili.StateFailed:
			// 失败就停在窗口上让用户看清原因，由用户决定重试还是关掉
			return
		}

		if !s.Running {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// 从浏览器导入

// loginFromBrowser 列出本机浏览器配置，让用户挑一个导入。
func (w *Win) loginFromBrowser() {
	profiles := cookie.Discover()
	if len(profiles) == 0 {
		w.log("没找到本机浏览器配置")
		walk.MsgBox(w.mw, "没有可导入的浏览器",
			"没在本机找到 Chromium 系（Chrome / Edge / Brave 等）的浏览器配置。\n\n"+
				"可以改用扫码登录；或先用那个浏览器登录一次 bilibili.com 再回来。",
			walk.MsgBoxIconInformation)
		return
	}

	type row struct {
		profile cookie.Profile
		label   string
		usable  bool
	}
	rows := make([]row, 0, len(profiles))
	for _, p := range profiles {
		r := row{profile: p}
		// HasBilibili 只读 SQLite、完全不解密：快，也不会把任何值取出来。
		// 用它给每一项标一个「有没有登录」，免得用户一个个去试。
		if has, err := p.HasBilibili(); err != nil {
			r.label = fmt.Sprintf("%s　读取失败：浏览器可能正开着", p.Display())
		} else if has {
			r.label = fmt.Sprintf("%s　有 B 站登录", p.Display())
			r.usable = true
		} else {
			r.label = fmt.Sprintf("%s　没有 B 站登录", p.Display())
		}
		rows = append(rows, r)
	}

	labels := make([]string, 0, len(rows))
	for _, r := range rows {
		labels = append(labels, r.label)
	}

	var (
		dlg    *walk.Dialog
		lb     *walk.ListBox
		status *walk.Label
	)
	selected := -1
	for i, r := range rows {
		if r.usable {
			selected = i
			break
		}
	}

	_, err := declarative.Dialog{
		AssignTo: &dlg,
		Title:    "从浏览器导入登录",
		Size:     declarative.Size{Width: 520, Height: 330},
		Layout:   declarative.VBox{Spacing: 7},
		Children: []declarative.Widget{
			declarative.Label{Text: "选一个浏览器配置，把它里面已登录的 bilibili.com Cookie 导进来。"},
			declarative.Label{Text: "提示：浏览器正开着时数据库会被锁住，必要时先关掉浏览器。"},
			declarative.ListBox{
				AssignTo:     &lb,
				Model:        labels,
				CurrentIndex: selected,
			},
			declarative.Label{AssignTo: &status, Text: ""},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 6},
				Children: []declarative.Widget{
					declarative.PushButton{
						Text:    "导入",
						MinSize: declarative.Size{Width: 96},
						OnClicked: func() {
							i := lb.CurrentIndex()
							if i < 0 || i >= len(rows) {
								status.SetText("请先选择一个浏览器配置")
								return
							}
							if !rows[i].usable {
								status.SetText(rows[i].label)
								return
							}
							status.SetText("正在读取并校验…")
							go w.importBrowserProfile(dlg, rows[i].profile, status)
						},
					},
					declarative.HSpacer{},
					declarative.PushButton{
						Text:      "取消",
						MinSize:   declarative.Size{Width: 80},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}.Run(w.mw)
	if err != nil {
		w.log("登录窗口打开失败：%v", err)
	}
}

// importBrowserProfile 在后台读 cookie 并校验，成功就关窗。
func (w *Win) importBrowserProfile(dlg *walk.Dialog, p cookie.Profile, status *walk.Label) {
	fail := func(msg string) {
		dlg.Synchronize(func() { status.SetText(msg) })
		w.log("✗ %s：%s", p.Display(), msg)
	}

	ck, err := p.BilibiliCookie()
	if err != nil {
		switch {
		case errors.Is(err, cookie.ErrNoBilibiliLogin):
			fail("这个配置里没有 B 站 Cookie。请先用那个浏览器登录一次 bilibili.com。")
		case errors.Is(err, cookie.ErrAppBound):
			fail("Cookie 用了应用绑定加密（Chrome 127+ 的新格式），无法解密。请改用扫码登录。")
		default:
			fail("读取失败：" + err.Error() + "（浏览器正开着占用数据库是常见原因）")
		}
		return
	}

	res := w.verifyCookie(ck, "浏览器 "+p.Display())
	if res == nil {
		dlg.Synchronize(func() { status.SetText("Cookie 已经失效，请重新登录那个浏览器") })
		return
	}
	dlg.Synchronize(func() { dlg.Accept() })
	w.applyLogin(res)
}

// ---------------------------------------------------------------------------
// 导入凭证文件 / 文本

// loginFromFile 提供一个粘贴框并支持直接选文件。
//
// 主要用途是接住 BBDown 老用户手上的 BBDown.data（一行 cookie）与
// BBDownTV.data（access_token），不用重新登录一次。
func (w *Win) loginFromFile() {
	var (
		dlg    *walk.Dialog
		te     *walk.TextEdit
		status *walk.Label
	)

	doImport := func(text, source string) {
		cred := bilibili.ParseCredentialText(text)
		if cred.Empty() {
			msg := "没找到可用的凭证。需要 SESSDATA=…（网页端）或 access_token（电视端）。"
			status.SetText(msg)
			w.log("✗ %s：没找到 SESSDATA 或 access_token", source)
			return
		}
		// 只有 access_token（BBDownTV.data）时没法用 nav 校验，直接存下来。
		if !bilibili.HasAuthCookie(cred.Cookie) && cred.Token != "" {
			w.cfg.AccessToken = cred.Token
			w.saveConfig()
			w.runner.SetCredentials(w.cfg.Cookie, cred.Token)
			w.log("✓ 已导入电视端凭证（%s）", source)
			dlg.Accept()
			return
		}
		status.SetText("正在校验…")
		go func() {
			res := w.verifyCookie(cred.Cookie, source)
			if res == nil {
				dlg.Synchronize(func() { status.SetText("Cookie 已经失效了，请重新登录一次再导入") })
				return
			}
			if cred.Token != "" {
				res.Token = cred.Token
			}
			dlg.Synchronize(func() { dlg.Accept() })
			w.applyLogin(res)
		}()
	}

	_, err := declarative.Dialog{
		AssignTo: &dlg,
		Title:    "导入凭证文件 / 粘贴内容",
		Size:     declarative.Size{Width: 620, Height: 360},
		Layout:   declarative.VBox{Spacing: 7},
		Children: []declarative.Widget{
			declarative.Label{Text: "把 BBDown.data 的内容粘进来，或直接选文件。"},
			declarative.Label{Text: "BBDown.data 是一行 SESSDATA=…; bili_jct=…；BBDownTV.data 是一串 access_token。"},
			declarative.TextEdit{
				AssignTo: &te,
				VScroll:  true,
				MinSize:  declarative.Size{Height: 150},
			},
			declarative.Label{AssignTo: &status, Text: ""},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 6},
				Children: []declarative.Widget{
					declarative.PushButton{
						Text:    "选择文件…",
						MinSize: declarative.Size{Width: 104},
						OnClicked: func() {
							fd := new(walk.FileDialog)
							fd.Title = "选择凭证文件"
							fd.Filter = "BBDown 凭证 (*.data)|*.data|所有文件 (*.*)|*.*"
							if ok, err := fd.ShowOpen(dlg); err != nil {
								status.SetText("打开文件对话框失败：" + err.Error())
							} else if ok {
								// 只读进来填到框里，不直接提交 —— 让用户能看到
								// 自己导入的到底是什么，再决定要不要用。
								raw, err := os.ReadFile(fd.FilePath)
								if err != nil {
									status.SetText("读取失败：" + err.Error())
									return
								}
								te.SetText(strings.TrimSpace(string(raw)))
								status.SetText("已读取 " + fileBase(fd.FilePath) + "，确认无误后点「导入」")
							}
						},
					},
					declarative.HSpacer{},
					declarative.PushButton{
						Text:    "导入",
						MinSize: declarative.Size{Width: 96},
						OnClicked: func() {
							txt := te.Text()
							if strings.TrimSpace(txt) == "" {
								status.SetText("请先粘贴内容或选择一个文件")
								return
							}
							doImport(txt, "粘贴的文本")
						},
					},
					declarative.PushButton{
						Text:      "取消",
						MinSize:   declarative.Size{Width: 80},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}.Run(w.mw)
	if err != nil {
		w.log("登录窗口打开失败：%v", err)
	}
}

// ---------------------------------------------------------------------------
// 账号密码

// loginByPassword 用账号密码登录。
//
// 走的是**电视端**接口（/x/passport-tv-login/login），不是网页端 ——
// 网页端那套强制要求先过极验滑块，而极验必须由真实浏览器执行 JS 才能通过，
// 本项目零依赖，过不去。电视端接口完全不校验极验。
func (w *Win) loginByPassword() {
	var (
		dlg    *walk.Dialog
		userLE *walk.LineEdit
		passLE *walk.LineEdit
		status *walk.Label
		loginB *walk.PushButton
	)

	doLogin := func() {
		user := strings.TrimSpace(userLE.Text())
		pass := passLE.Text()
		if user == "" {
			status.SetText("请填写账号（手机号 / 邮箱）")
			return
		}
		if pass == "" {
			status.SetText("请填写密码")
			return
		}
		loginB.SetEnabled(false)
		status.SetText("正在登录…")

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			res, err := w.runner.Client().LoginWithPassword(ctx, user, pass)
			if err != nil {
				msg := err.Error()
				dlg.Synchronize(func() {
					loginB.SetEnabled(true)
					// 密码不进日志、不回显 —— 连失败日志里也不带它。
					status.SetText(msg)
				})
				w.log("✗ 账号密码登录失败：%v", err)
				return
			}
			dlg.Synchronize(func() { dlg.Accept() })
			w.applyLogin(res)
		}()
	}

	_, err := declarative.Dialog{
		AssignTo: &dlg,
		Title:    "账号密码登录",
		Size:     declarative.Size{Width: 430, Height: 250},
		Layout:   declarative.VBox{Spacing: 8},
		Children: []declarative.Widget{
			declarative.Label{Text: "用 B 站账号密码登录（走电视端接口，不弹极验滑块）。"},
			declarative.Label{Text: "密码只用于这一次登录，不会写进配置文件，也不会记进日志。"},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 6},
				Children: []declarative.Widget{
					declarative.Label{Text: "账号", MinSize: declarative.Size{Width: 44}},
					declarative.LineEdit{AssignTo: &userLE, CueBanner: "手机号或邮箱"},
				},
			},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 6},
				Children: []declarative.Widget{
					declarative.Label{Text: "密码", MinSize: declarative.Size{Width: 44}},
					declarative.LineEdit{AssignTo: &passLE, PasswordMode: true},
				},
			},
			declarative.Label{AssignTo: &status, Text: ""},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 6},
				Children: []declarative.Widget{
					declarative.PushButton{
						AssignTo:  &loginB,
						Text:      "登录",
						MinSize:   declarative.Size{Width: 96},
						OnClicked: doLogin,
					},
					declarative.HSpacer{},
					declarative.PushButton{
						Text:      "取消",
						MinSize:   declarative.Size{Width: 80},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}.Run(w.mw)
	if err != nil {
		w.log("登录窗口打开失败：%v", err)
	}
}

// ---------------------------------------------------------------------------
// 落盘

// verifyCookie 校验一段 cookie，通过返回可直接落盘的登录结果。
//
// 这是浏览器导入 / 文件导入两条通道共用的入口 —— 它们拿到的都是一段
// 现成的 cookie，差别只在来源描述。校验不过返回 nil，失败原因已写进日志。
func (w *Win) verifyCookie(ck, source string) *bilibili.LoginResult {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	acc, err := w.runner.Client().Verify(ctx, ck)
	if err != nil {
		w.log("✗ %s：校验出错：%v", source, err)
		return nil
	}
	if acc == nil || !acc.LoggedIn {
		msg := "凭证已失效"
		if acc != nil && acc.Message != "" {
			msg = acc.Message
		}
		w.log("✗ %s：%s", source, msg)
		return nil
	}
	w.log("✓ 从 %s 登录成功：%s", source, acc.UName)
	return &bilibili.LoginResult{Cookie: ck, MID: acc.MID, UName: acc.UName}
}

// applyLogin 把登录结果落盘并刷新界面。五条通道最后都汇到这里。
//
// 已经失效的 access_token 会被换掉：config.json 里存着一个过期的电视端
// token 时，界面会显示"已登录"但取流仍然按未登录处理，非常难排查 ——
// 既然刚刚拿到了新的凭证，就顺手把旧的一起更新掉。
func (w *Win) applyLogin(res *bilibili.LoginResult) {
	if res == nil {
		return
	}
	if res.Cookie != "" {
		w.cfg.Cookie = res.Cookie
	}
	if res.Token != "" {
		w.cfg.AccessToken = res.Token
	}
	if res.UName != "" {
		w.cfg.UName = res.UName
	}
	if res.MID != 0 {
		w.cfg.MID = res.MID
	}
	w.saveConfig()
	w.runner.SetCredentials(w.cfg.Cookie, w.cfg.AccessToken)

	name := res.UName
	if name == "" {
		name = "（昵称获取失败，但凭证已保存）"
	}
	w.setAccount("已登录："+name, toneOk)
	w.log("登录成功：%s", name)
}

// fileBase 只取文件名，避免把用户的完整路径写进日志。
func fileBase(path string) string {
	path = strings.TrimRight(path, `/\`)
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

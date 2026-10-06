package gui

import (
	"fmt"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"bbdown-native/internal/bilibili"
)

// loginDialog 是扫码登录窗口。
//
// 它自己不碰网络与状态机 —— 状态全在 bilibili.LoginSession 里，
// 这里只负责「每 400ms 取一次快照，变了就重画」。
type loginDialog struct {
	dlg    *walk.Dialog
	sess   *bilibili.LoginSession
	img    *walk.ImageView
	status *walk.Label
	remain *walk.Label
	urlLB  *walk.LineEdit
	modeCB *walk.ComboBox

	// 退出时回填的结果
	result *bilibili.LoginResult
}

// 登录方式下拉。
var loginModes = []struct {
	Label string
	Mode  bilibili.LoginMode
}{
	{"网页端（凭证约 30 天）", bilibili.LoginWeb},
	{"电视端（凭证约 180 天）", bilibili.LoginTV},
}

func (w *Win) onLogin() {
	d := &loginDialog{sess: w.login}

	_, err := Dialog{
		AssignTo:      &d.dlg,
		Title:         "扫码登录",
		FixedSize:     true,
		MinSize:       Size{Width: 380, Height: 520},
		Size:          Size{Width: 380, Height: 520},
		Layout:        VBox{Spacing: 6},
		DefaultButton: nil,
		Children: []Widget{
			ComboBox{
				AssignTo:     &d.modeCB,
				Model:        []string{loginModes[0].Label, loginModes[1].Label},
				CurrentIndex: 0,
				OnCurrentIndexChanged: func() {
					// 换登录方式就重开一轮
					if d.sess.Snapshot().Running {
						d.sess.Cancel()
					}
					d.restart()
				},
			},
			Composite{
				Layout: VBox{MarginsZero: true},
				Children: []Widget{
					// 二维码区域固定白底 —— 二维码必须白底黑码，
					// 深色主题下也不能反色，否则扫码率会掉。
					ImageView{
						AssignTo: &d.img,
						MinSize:  Size{Width: 320, Height: 320},
						MaxSize:  Size{Width: 320, Height: 320},
						Mode:     ImageViewModeZoom,
						Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)},
					},
				},
			},
			Label{AssignTo: &d.status, Text: "正在申请二维码…", Alignment: AlignHCenterVCenter},
			Label{AssignTo: &d.remain, Text: "", Alignment: AlignHCenterVCenter},
			LineEdit{
				AssignTo: &d.urlLB,
				ReadOnly: true,
				CueBanner: "二维码里的链接（一般不用管）",
			},
			Composite{
				Layout: HBox{Spacing: 6},
				Children: []Widget{
					PushButton{
						Text:      "重新获取",
						MinSize:   Size{Width: 96},
						OnClicked: func() { d.sess.Cancel(); d.restart() },
					},
					HSpacer{},
					PushButton{
						Text:      "取消",
						MinSize:   Size{Width: 80},
						OnClicked: func() { d.sess.Cancel(); d.dlg.Cancel() },
					},
				},
			},
		},
	}.Run(w.mw)
	if err != nil {
		w.log("登录窗口打开失败：%v", err)
		return
	}

	// 窗口关掉后一定要停掉后台轮询
	d.sess.Cancel()

	if d.result != nil {
		w.applyLogin(d.result)
	}
}

// restart 按当前选中的方式重新开始一轮。
func (d *loginDialog) restart() {
	mode := loginModes[d.modeCB.CurrentIndex()].Mode
	ok, msg := d.sess.Start(mode, nil)
	if !ok {
		d.status.SetText(msg)
		return
	}
	d.status.SetText("正在申请二维码…")
	d.remain.SetText("")
	d.urlLB.SetText("")
	go d.pump()
}

// pump 轮询快照，把变化画到界面上；成功后关窗。
func (d *loginDialog) pump() {
	lastSeq := -1
	lastImg := -1
	for {
		if d.dlg == nil {
			return
		}
		s := d.sess.Snapshot()
		if s.Seq != lastSeq {
			lastSeq = s.Seq
			snap := s
			d.dlg.Synchronize(func() {
				d.status.SetText(snap.Message)
				if snap.URL != "" {
					d.urlLB.SetText(snap.URL)
				}
			})
		}
		if s.Remain > 0 {
			r := s.Remain
			d.dlg.Synchronize(func() { d.remain.SetText(fmt.Sprintf("%d 秒后失效", r)) })
		}

		// 二维码换了才重建位图，别每轮都造一张
		if s.Seq != lastImg {
			if img := d.sess.QRImage(); img != nil {
				lastImg = s.Seq
				bm, err := walk.NewBitmapFromImage(img)
				if err != nil {
					d.dlg.Synchronize(func() { d.status.SetText("二维码渲染失败：" + err.Error()) })
				} else {
					d.dlg.Synchronize(func() { _ = d.img.SetImage(bm) })
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

// applyLogin 把登录结果落盘并刷新界面。
func (w *Win) applyLogin(res *bilibili.LoginResult) {
	if res.Cookie != "" {
		w.cfg.Cookie = res.Cookie
	}
	if res.Token != "" {
		w.cfg.AccessToken = res.Token
	}
	w.cfg.UName = res.UName
	if res.MID != 0 {
		w.cfg.MID = res.MID
	}
	if err := w.cfg.Save(); err != nil {
		w.log("凭证没能保存：%v", err)
	}
	w.runner.SetCredentials(res.Cookie, res.Token)

	name := res.UName
	if name == "" {
		name = "（昵称获取失败，但凭证已保存）"
	}
	w.acctLB.SetText("已登录：" + name)
	w.log("登录成功：%s　凭证已保存到 %s", name, w.cfg.Path())
}

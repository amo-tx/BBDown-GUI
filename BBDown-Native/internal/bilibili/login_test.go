package bilibili

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"testing"
)

func TestNormalizeCookie(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空", "", ""},
		{"普通", "SESSDATA=abc; bili_jct=def", "SESSDATA=abc; bili_jct=def"},
		{"带 Cookie 前缀", "Cookie: SESSDATA=abc; bili_jct=def", "SESSDATA=abc; bili_jct=def"},
		{"小写前缀", "cookie:SESSDATA=abc", "SESSDATA=abc"},
		{"换行分隔", "SESSDATA=abc\nbili_jct=def\r\nDedeUserID=1",
			"SESSDATA=abc; bili_jct=def; DedeUserID=1"},
		{"制表符", "SESSDATA=abc\tbili_jct=def", "SESSDATA=abc; bili_jct=def"},
		{"同名保留最后一个", "SESSDATA=old; bili_jct=x; SESSDATA=new",
			"SESSDATA=new; bili_jct=x"},
		{"丢弃无等号片段", "SESSDATA=abc; junk; bili_jct=d", "SESSDATA=abc; bili_jct=d"},
		{"丢弃空键", "=v; SESSDATA=abc", "SESSDATA=abc"},
		{"值里含等号", "SESSDATA=a=b; bili_jct=c", "SESSDATA=a=b; bili_jct=c"},
		{"前后空白", "  SESSDATA=abc  ;  bili_jct=def  ", "SESSDATA=abc; bili_jct=def"},
		{"值里的空格保留", "SESSDATA=a b; bili_jct=c", "SESSDATA=a b; bili_jct=c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeCookie(c.in); got != c.want {
				t.Errorf("NormalizeCookie(%q)\n 得到 %q\n 期望 %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeCookieIdempotent(t *testing.T) {
	in := "Cookie: SESSDATA=abc;bili_jct=def\nDedeUserID=9"
	once := NormalizeCookie(in)
	twice := NormalizeCookie(once)
	if once != twice {
		t.Errorf("不幂等:\n 一次 %q\n 二次 %q", once, twice)
	}
}

func TestCookieToMap(t *testing.T) {
	m := CookieToMap("SESSDATA=abc; bili_jct=def; broken; =x")
	if len(m) != 2 || m["SESSDATA"] != "abc" || m["bili_jct"] != "def" {
		t.Errorf("解析结果不对: %#v", m)
	}
	if HasAuthCookie("bili_jct=def") {
		t.Error("没有 SESSDATA 不应算已鉴权")
	}
	if !HasAuthCookie("bili_jct=def; SESSDATA=x") {
		t.Error("有 SESSDATA 应算已鉴权")
	}
}

func TestJoinCookiesKeyFirstThenSorted(t *testing.T) {
	got := joinCookies(map[string]string{
		"zzz":      "1",
		"aaa":      "2",
		"SESSDATA": "s",
		"bili_jct": "j",
	})
	want := "SESSDATA=s; bili_jct=j; aaa=2; zzz=1"
	if got != want {
		t.Errorf("得到 %q，期望 %q", got, want)
	}
	// 空值必须被丢掉
	if s := joinCookies(map[string]string{"SESSDATA": "", "bili_jct": "j"}); s != "bili_jct=j" {
		t.Errorf("空值未被丢弃: %q", s)
	}
	// 稳定性：同一集合多次拼接结果一致
	a := joinCookies(map[string]string{"b": "2", "a": "1", "c": "3"})
	for i := 0; i < 20; i++ {
		if b := joinCookies(map[string]string{"c": "3", "a": "1", "b": "2"}); b != a {
			t.Fatalf("拼接结果不稳定: %q vs %q", a, b)
		}
	}
}

func TestTVSignDeterministicShape(t *testing.T) {
	signed := tvSign(url.Values{"auth_code": {"abc"}})
	for _, k := range []string{"appkey", "local_id", "ts", "sign", "auth_code"} {
		if signed.Get(k) == "" {
			t.Errorf("缺少字段 %q", k)
		}
	}
	if signed.Get("appkey") != tvAppKey {
		t.Errorf("appkey 不对: %q", signed.Get("appkey"))
	}
	if signed.Get("local_id") != "0" {
		t.Errorf("local_id 应为 0，得到 %q", signed.Get("local_id"))
	}
	if len(signed.Get("sign")) != 32 {
		t.Errorf("sign 应为 32 位 md5 十六进制，得到 %q", signed.Get("sign"))
	}

	// 按规范手工复算一遍，确认签的是「排除 sign 后的字典序拼接 + 盐」
	p := url.Values{
		"appkey":    {tvAppKey},
		"local_id":  {"0"},
		"ts":        {signed.Get("ts")},
		"auth_code": {"abc"},
	}
	sum := md5.Sum([]byte(p.Encode() + tvAppSec))
	if want := hex.EncodeToString(sum[:]); signed.Get("sign") != want {
		t.Errorf("sign 与规范不符:\n 得到 %q\n 期望 %q", signed.Get("sign"), want)
	}
	// 参数参与签名：换个 auth_code，sign 必须变
	if tvSign(url.Values{"auth_code": {"xyz"}}).Get("sign") == signed.Get("sign") {
		t.Error("auth_code 未参与签名")
	}
}

func TestLoginMode(t *testing.T) {
	if ParseLoginMode("tv") != LoginTV || ParseLoginMode("TV") != LoginTV || ParseLoginMode(" tv ") != LoginTV {
		t.Error("tv 解析失败")
	}
	if ParseLoginMode("web") != LoginWeb || ParseLoginMode("") != LoginWeb || ParseLoginMode("x") != LoginWeb {
		t.Error("非 tv 一律应回落到 web")
	}
	if LoginTV.String() != "tv" || LoginWeb.String() != "web" {
		t.Error("String 不对")
	}
}

func TestLoginStateString(t *testing.T) {
	want := map[LoginState]string{
		StateIdle: "idle", StateLoading: "loading", StatePending: "pending",
		StateScanned: "scanned", StateConfirmed: "confirmed",
		StateExpired: "expired", StateFailed: "error",
	}
	for st, w := range want {
		if got := st.String(); got != w {
			t.Errorf("状态 %d 得到 %q，期望 %q", st, got, w)
		}
	}
}

func TestSessionIdleSnapshot(t *testing.T) {
	s := NewLoginSession()
	snap := s.Snapshot()
	if snap.State != StateIdle || snap.Running {
		t.Errorf("新会话应处于 idle 且未运行，得到 %+v", snap)
	}
	if s.QRImage() != nil {
		t.Error("新会话不该有二维码")
	}
	// 取消一个未启动的会话不应 panic
	s.Cancel()
}

func TestRenderQRProducesScannableShape(t *testing.T) {
	img := renderQR("https://example.com/hello")
	if img == nil {
		t.Fatal("二维码渲染失败")
	}
	b := img.Bounds()
	if b.Dx() != qrImageSizePx || b.Dy() != qrImageSizePx {
		t.Errorf("尺寸应为 %dx%d，得到 %dx%d", qrImageSizePx, qrImageSizePx, b.Dx(), b.Dy())
	}
	// 必须含黑白两种像素，否则等于没画出内容。
	//
	// 阈值只用来挡「退化成整片实心」这种情况（BBDown 自带 login 在控制台
	// 画字符画就是这个毛病）。正常二维码含 4 模块静默区，黑块占比约 25%~45%。
	var dark, light int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, _, _, _ := img.At(x, y).RGBA()
			if r > 0x8000 {
				light++
			} else {
				dark++
			}
		}
	}
	if dark == 0 || light == 0 {
		t.Fatalf("二维码像素分布异常: 黑=%d 白=%d", dark, light)
	}
	if ratio := float64(dark) / float64(dark+light); ratio > 0.60 || ratio < 0.10 {
		t.Errorf("黑块占比 %.1f%% 不像正常二维码（正常约 25%%~45%%）", ratio*100)
	}
}

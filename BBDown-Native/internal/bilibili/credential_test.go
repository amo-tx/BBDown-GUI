package bilibili

import (
	"os"
	"strings"
	"testing"
)

// 真实 BBDown.data 的形态：单行、标准 cookie 串。
//
// SESSDATA 里刻意用**字面逗号**：这是最容易踩的坑 —— 值的字符类一旦
// 把逗号当分隔符，就会被截断成一个看似正常、实则永远过不了鉴权的值。
// （B 站实际下发的是 `%2C` 编码，也有客户端会原样带逗号，两种都要保住。）
const realBBDownData = "SESSDATA=abcd,1700000000,xyz12*ab; DedeUserID=1234567; " +
	"bili_jct=0123456789abcdef0123456789abcdef; DedeUserID__ckMd5=0123456789abcdef; sid=abc123xy"

func TestParseCredentialBBDownData(t *testing.T) {
	c := ParseCredentialText(realBBDownData)
	if !HasAuthCookie(c.Cookie) {
		t.Fatalf("应当抽到 SESSDATA，实际 cookie=%q", c.Cookie)
	}
	if c.Token != "" {
		t.Errorf("这段里没有 token，不该抽出 %q", c.Token)
	}
	if !c.Usable() {
		t.Error("有 SESSDATA 就应当算可用")
	}
	m := CookieToMap(c.Cookie)
	if m["SESSDATA"] != "abcd,1700000000,xyz12*ab" {
		t.Errorf("SESSDATA 被改动了：%q", m["SESSDATA"])
	}
	// 逗号与星号必须原样保留
	if !strings.Contains(m["SESSDATA"], ",") || !strings.Contains(m["SESSDATA"], "*") {
		t.Errorf("SESSDATA 的逗号/星号被吃掉了：%q", m["SESSDATA"])
	}
}

// 百分号编码的形态（B 站实际下发的样子）同样不能动。
func TestParseCredentialEncodedSESSDATA(t *testing.T) {
	c := ParseCredentialText("SESSDATA=abcd%2C1700000000%2Cxyz12%2Aab; bili_jct=jj")
	m := CookieToMap(c.Cookie)
	if m["SESSDATA"] != "abcd%2C1700000000%2Cxyz12%2Aab" {
		t.Errorf("百分号编码的 SESSDATA 被改动了：%q", m["SESSDATA"])
	}
}

// 字段名大小写不同也要认，且要纠正成官方写法。
func TestParseCredentialCaseInsensitive(t *testing.T) {
	c := ParseCredentialText("sessdata=abc; BILI_JCT=def; dedeuserid=42")
	if !HasAuthCookie(c.Cookie) {
		t.Fatalf("小写 sessdata 应当被纠正为 SESSDATA，实际 %q", c.Cookie)
	}
	m := CookieToMap(c.Cookie)
	if m["SESSDATA"] != "abc" || m["bili_jct"] != "def" || m["DedeUserID"] != "42" {
		t.Errorf("键名没被规范成官方大小写：%v", m)
	}
}

func TestParseCredentialMultiLine(t *testing.T) {
	text := "SESSDATA=aaa\nbili_jct=bbb\nDedeUserID=7\n"
	c := ParseCredentialText(text)
	m := CookieToMap(c.Cookie)
	if m["SESSDATA"] != "aaa" || m["bili_jct"] != "bbb" || m["DedeUserID"] != "7" {
		t.Errorf("多行 cookie 没解析出来：%v", m)
	}
}

func TestParseCredentialWithCookiePrefix(t *testing.T) {
	c := ParseCredentialText("Cookie: SESSDATA=aaa; bili_jct=bbb")
	if CookieToMap(c.Cookie)["SESSDATA"] != "aaa" {
		t.Errorf("带 Cookie: 前缀的没解析出来：%q", c.Cookie)
	}
}

// 带 BOM 的文件（记事本另存为的默认产物）。
func TestParseCredentialBOM(t *testing.T) {
	c := ParseCredentialText("\ufeff" + realBBDownData)
	if !HasAuthCookie(c.Cookie) {
		t.Fatalf("带 BOM 的 cookie 应当能被解析，实际 %q", c.Cookie)
	}
	if strings.Contains(c.Cookie, "\ufeff") {
		t.Error("BOM 不该残留在结果里")
	}
}

func TestParseCredentialJSONCookieString(t *testing.T) {
	c := ParseCredentialText(`{"cookie":"SESSDATA=aaa; bili_jct=bbb","uname":"某人"}`)
	if CookieToMap(c.Cookie)["SESSDATA"] != "aaa" {
		t.Errorf("JSON 里 cookie 字段没解析出来：%q", c.Cookie)
	}
}

func TestParseCredentialJSONFields(t *testing.T) {
	c := ParseCredentialText(`{"SESSDATA":"aaa","bili_jct":"bbb","DedeUserID":"9"}`)
	m := CookieToMap(c.Cookie)
	if m["SESSDATA"] != "aaa" || m["bili_jct"] != "bbb" || m["DedeUserID"] != "9" {
		t.Errorf("JSON 分散字段没拼起来：%v", m)
	}
}

func TestParseCredentialJSONToken(t *testing.T) {
	c := ParseCredentialText(`{"access_token":"` + fakeToken + `","mid":5}`)
	if c.Token != fakeToken {
		t.Errorf("JSON 里的 access_token 没抽出来：%q", c.Token)
	}
}

// 裸 token：整段就是一个长串，没有键名。
const fakeToken = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH"

func TestParseCredentialBareToken(t *testing.T) {
	c := ParseCredentialText(fakeToken + "\n")
	if c.Token != fakeToken {
		t.Errorf("裸 token 没被识别：%q", c.Token)
	}
	if c.Cookie != "" {
		t.Errorf("裸 token 不该被当成 cookie：%q", c.Cookie)
	}
	if !c.Usable() {
		t.Error("有 token 就应当算可用")
	}
}

func TestParseCredentialTokenWithKey(t *testing.T) {
	c := ParseCredentialText("access_token=" + fakeToken)
	if c.Token != fakeToken {
		t.Errorf("access_token= 形式没抽出来：%q", c.Token)
	}
}

// 混杂文本：前面有日志、后面才是 cookie。
func TestParseCredentialScavengeFromNoise(t *testing.T) {
	text := "登录成功\n账号：某人\ncookie 已保存\n" + realBBDownData + "\n谢谢使用"
	c := ParseCredentialText(text)
	if !HasAuthCookie(c.Cookie) {
		t.Fatalf("从噪声文本里没捞到 SESSDATA：%q", c.Cookie)
	}
}

func TestParseCredentialEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n", "\ufeff"} {
		c := ParseCredentialText(in)
		if !c.Empty() || c.Usable() {
			t.Errorf("输入 %q 应当得到空凭证，实际 %+v", in, c)
		}
	}
}

// 完全无关的文本不该被硬当成凭据。
func TestParseCredentialGarbage(t *testing.T) {
	c := ParseCredentialText("这是一段完全无关的文字，没有任何凭据信息。")
	if c.Usable() {
		t.Errorf("无关文本不该产出可用凭证：%+v", c)
	}
}

// 没有 SESSDATA 的 cookie 串一律视为无效 —— 顺手凑出一堆无关字段
// 只会让上层的报错更难懂。
func TestParseCredentialNoSESSDATA(t *testing.T) {
	for _, in := range []string{"foo=bar; baz=qux", "buvid3=abc", "bili_jct=only"} {
		c := ParseCredentialText(in)
		if c.Cookie != "" {
			t.Errorf("输入 %q 没有 SESSDATA，cookie 应当为空，实际 %q", in, c.Cookie)
		}
		if c.Usable() {
			t.Errorf("输入 %q 不该算出可用凭证", in)
		}
	}
}

// 认得 SESSDATA 时，额外字段也要一并保留。
func TestParseCredentialKeepsExtraFields(t *testing.T) {
	text := "SESSDATA=aaa; bili_jct=bbb; buvid3=extra-value; i-wanna-go-back=-1"
	c := ParseCredentialText(text)
	m := CookieToMap(c.Cookie)
	for k, want := range map[string]string{
		"SESSDATA":        "aaa",
		"bili_jct":        "bbb",
		"buvid3":          "extra-value",
		"i-wanna-go-back": "-1",
	} {
		if m[k] != want {
			t.Errorf("字段 %s = %q，期望 %q（完整：%q）", k, m[k], want, c.Cookie)
		}
	}
}

// 短串不该被误判成裸 token。
func TestParseCredentialShortStringNotToken(t *testing.T) {
	c := ParseCredentialText("hello")
	if c.Token != "" {
		t.Errorf("短的普通字符串不该当成 token：%q", c.Token)
	}
}

// cookie 与 token 同时存在。
func TestParseCredentialBoth(t *testing.T) {
	c := ParseCredentialText(`{"cookie":"SESSDATA=aaa","access_token":"` + fakeToken + `"}`)
	if !HasAuthCookie(c.Cookie) || c.Token != fakeToken {
		t.Errorf("cookie 与 token 应当都抽到，实际 %+v", c)
	}
}

// URL 查询串形态：`SESSDATA=x&bili_jct=y`。& 必须当分隔符。
func TestParseCredentialURLQuery(t *testing.T) {
	c := ParseCredentialText("SESSDATA=aaa&bili_jct=bbb")
	m := CookieToMap(c.Cookie)
	if m["SESSDATA"] != "aaa" || m["bili_jct"] != "bbb" {
		t.Errorf("URL 查询串没切开：%v", m)
	}
}

// 拿本机真实的 BBDown.data 验一遍解析。
//
// 默认跳过：仓库里不能依赖用户机器上有这个文件。要手动验就跑
//
//	BBDOWN_CRED_FILE=C:\path\to\BBDown.data go test ./internal/bilibili/ -run RealFile -v
//
// 它只断言「抽出来的 SESSDATA 长度合理」—— 绝不打印凭据本身，
// 免得测试输出里留下一份可用的凭证。
func TestParseCredentialRealFile(t *testing.T) {
	path := os.Getenv("BBDOWN_CRED_FILE")
	if path == "" {
		t.Skip("未设置 BBDOWN_CRED_FILE，跳过真实文件测试")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	c := ParseCredentialText(string(raw))
	if !HasAuthCookie(c.Cookie) {
		t.Fatalf("真实文件里没抽到 SESSDATA（文件 %d 字节）", len(raw))
	}
	sd := CookieToMap(c.Cookie)["SESSDATA"]
	if len(sd) < 30 {
		t.Errorf("SESSDATA 只有 %d 字节，短得不像真的", len(sd))
	}
	// 真实 SESSDATA 一定带分隔符（`,` 或 `%2C`）。没有就说明被截断了。
	if !strings.Contains(sd, ",") && !strings.Contains(sd, "%2C") {
		t.Error("SESSDATA 里没有分隔符，多半被截断了")
	}
	t.Logf("抽到 %d 个 cookie 字段，SESSDATA %d 字节", len(CookieToMap(c.Cookie)), len(sd))
}

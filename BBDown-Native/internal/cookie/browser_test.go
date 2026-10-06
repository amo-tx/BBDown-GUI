package cookie

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"
	"time"
)

func TestIsBilibiliHost(t *testing.T) {
	yes := []string{
		"bilibili.com", ".bilibili.com", "www.bilibili.com",
		".api.bilibili.com", "passport.bilibili.com",
	}
	for _, h := range yes {
		if !isBilibiliHost(h) {
			t.Errorf("%q 应当被认作 B 站域名", h)
		}
	}
	no := []string{
		"", "example.com",
		"notbilibili.com",      // 后缀不同
		"bilibili.com.evil.cn", // 把 B 站域名当前缀
		"xbilibili.com",
		"bili.com",
	}
	for _, h := range no {
		if isBilibiliHost(h) {
			t.Errorf("%q 不该被认作 B 站域名", h)
		}
	}
}

func TestPreferCookie(t *testing.T) {
	parent := cookieRow{name: "SESSDATA", host: ".bilibili.com", expires: 100}
	sub := cookieRow{name: "SESSDATA", host: "www.bilibili.com", expires: 200}
	if !preferCookie(parent, sub) {
		t.Error("父域应当优先于子域（父域对 api.bilibili.com 也生效）")
	}
	if preferCookie(sub, parent) {
		t.Error("子域不该盖过父域")
	}

	older := cookieRow{host: ".bilibili.com", expires: 100}
	newer := cookieRow{host: ".bilibili.com", expires: 200}
	if !preferCookie(newer, older) {
		t.Error("同域时应当留过期时间更晚的")
	}
}

func TestIsExpired(t *testing.T) {
	// 0 表示会话 cookie，永远不算过期
	if isExpired(0) {
		t.Error("会话 cookie 不该被判为过期")
	}
	// 2000-01-01 左右（Chrome 时间戳，微秒，1601 起算）
	const y2000 = (11644473600 + 946684800) * 1_000_000
	if !isExpired(y2000) {
		t.Error("2000 年的过期时间应当算过期")
	}
	// 2099 年
	const y2099 = (11644473600 + 4070908800) * 1_000_000
	if isExpired(y2099) {
		t.Error("2099 年的过期时间不该算过期")
	}
	// 和 isExpired 用同一个 now 的口径核对一次边界
	now := time.Now().Unix()
	justExpired := (now - 10 + chromeEpochOffset) * 1_000_000
	if !isExpired(justExpired) {
		t.Error("10 秒前的过期时间应当算过期")
	}
	justValid := (now + 3600 + chromeEpochOffset) * 1_000_000
	if isExpired(justValid) {
		t.Error("1 小时后的过期时间不该算过期")
	}
}

// ---------------------------------------------------------------------------
// 解密

func seal(t *testing.T, key, plain []byte, aad string) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0x2a}, gcm.NonceSize())
	// gcm.Seal 的返回值是 ct||tag，并不含 nonce。而生产代码 decryptValue 期望
	// 的是 Chromium 的落盘布局 nonce||ct||tag（前缀 3 字节之外的那一段），
	// 所以这里必须把 nonce 拼回去 —— 不拼的话测试自己就少了 12 字节。
	// 注意 bytes.Repeat 返回的切片 cap == len，append 一定会另开一块内存，
	// 不会污染 nonce 本身。
	return append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, plain, []byte(aad))...)
}

func TestDecryptValueHostAsAAD(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	blob := append([]byte("v10"), seal(t, key, []byte("secret-value"), ".bilibili.com")...)

	got, err := decryptValue(key, blob, ".bilibili.com")
	if err != nil {
		t.Fatalf("解密失败：%v", err)
	}
	if got != "secret-value" {
		t.Errorf("解出 %q，期望 secret-value", got)
	}
}

// 老版本 Chrome 用空 AAD，不能只支持一种。
func TestDecryptValueEmptyAAD(t *testing.T) {
	key := bytes.Repeat([]byte{0x22}, 32)
	blob := append([]byte("v10"), seal(t, key, []byte("legacy"), "")...)

	got, err := decryptValue(key, blob, ".bilibili.com")
	if err != nil {
		t.Fatalf("空 AAD 的解密应当也能成功：%v", err)
	}
	if got != "legacy" {
		t.Errorf("解出 %q，期望 legacy", got)
	}
}

func TestDecryptValueV11(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 32)
	blob := append([]byte("v11"), seal(t, key, []byte("v11-value"), ".bilibili.com")...)
	got, err := decryptValue(key, blob, ".bilibili.com")
	if err != nil {
		t.Fatalf("v11 解密失败：%v", err)
	}
	if got != "v11-value" {
		t.Errorf("解出 %q", got)
	}
}

func TestDecryptValueAppBound(t *testing.T) {
	blob := append([]byte("v20"), bytes.Repeat([]byte{0}, 40)...)
	_, err := decryptValue(bytes.Repeat([]byte{1}, 32), blob, ".bilibili.com")
	if !errors.Is(err, ErrAppBound) {
		t.Errorf("v20 应当返回 ErrAppBound，实际 %v", err)
	}
}

func TestDecryptValueWrongKeyFails(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	blob := append([]byte("v10"), seal(t, key, []byte("x"), ".bilibili.com")...)
	if _, err := decryptValue(bytes.Repeat([]byte{0x55}, 32), blob, ".bilibili.com"); err == nil {
		t.Error("用错误的密钥应当解密失败")
	}
}

func TestDecryptValueTooShort(t *testing.T) {
	if _, err := decryptValue(bytes.Repeat([]byte{1}, 32), []byte("v10short"), "h"); err == nil {
		t.Error("长度不足的密文应当报错")
	}
}

// ---------------------------------------------------------------------------
// DPAPI 自检
//
// 这条测试直接验证「CryptProtectData → CryptUnprotectData」整条链路，
// 也就是验证 ReadProcessMemory 搬运 C 缓冲区的做法真的成立。
// 它是导入浏览器 Cookie 的全部基础，坏了必须第一时间知道。

func TestDPAPIRoundTrip(t *testing.T) {
	cases := [][]byte{
		[]byte("plain-ascii"),
		[]byte("含中文与符号 —— BBDown ✓"),
		bytes.Repeat([]byte("x"), 5000), // 顺带验一下大缓冲
	}
	for _, plain := range cases {
		enc, err := dpapiProtect(plain)
		if err != nil {
			t.Fatalf("加密失败：%v", err)
		}
		if len(enc) == 0 {
			t.Fatal("密文为空")
		}
		dec, err := dpapiUnprotect(enc)
		if err != nil {
			t.Fatalf("解密失败：%v", err)
		}
		if !bytes.Equal(dec, plain) {
			t.Errorf("往返不一致：长度 %d vs %d", len(dec), len(plain))
		}
	}
}

func TestDPAPIUnprotectGarbageFails(t *testing.T) {
	if _, err := dpapiUnprotect([]byte("这不是一段 DPAPI 密文")); err == nil {
		t.Error("乱码输入应当返回错误而不是崩掉")
	}
}

func TestDPAPIEmptyInput(t *testing.T) {
	got, err := dpapiUnprotect(nil)
	if err != nil {
		t.Fatalf("空输入不该报错：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("空输入应当返回空，实际 %d 字节", len(got))
	}
}

// ---------------------------------------------------------------------------
// 环境探测
//
// 这条不模拟：本机装了浏览器就读真库，没装就跳过。
// 它同时也是「表结构与真实浏览器是否一致」的回归闸门。

func TestDiscoverAndReadRealBrowser(t *testing.T) {
	profiles := Discover()
	if len(profiles) == 0 {
		t.Skip("本机没有可用的 Edge / Chrome 配置，跳过真实库测试")
	}
	t.Logf("发现 %d 个浏览器配置", len(profiles))

	for _, p := range profiles {
		tbl, err := ReadTable(p.CookiesDB, "cookies")
		if err != nil {
			// 浏览器正在运行可能锁住文件，这不算代码缺陷。
			t.Logf("%s：读取失败（可能被浏览器锁定）：%v", p.Display(), err)
			continue
		}
		t.Logf("%s：%d 行 cookie", p.Display(), len(tbl.Rows))
		if tbl.Column("host_key") < 0 || tbl.Column("encrypted_value") < 0 {
			t.Errorf("%s：真实库的列结构与预期不符：%v", p.Display(), tbl.Cols)
		}

		cookie, err := p.BilibiliCookie()
		switch {
		case err == nil:
			// 不打印 cookie 内容，只看有没有拿到关键项。
			if !bytes.Contains([]byte(cookie), []byte("SESSDATA=")) {
				t.Error("导出的 cookie 里没有 SESSDATA")
			}
			t.Logf("%s：成功导出 B 站 cookie（%d 字节）", p.Display(), len(cookie))
		case errors.Is(err, ErrNoBilibiliLogin):
			t.Logf("%s：库里没有 B 站登录态", p.Display())
		default:
			t.Logf("%s：导出失败：%v", p.Display(), err)
		}
	}
}

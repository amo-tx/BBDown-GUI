package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"bbdown-native/internal/app"
)

// newTestServer 起一个不落盘的实例：配置写在测试临时目录里，
// 绝不碰真实的 config.json。
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")
	ts := httptest.NewServer(s.middleware(s.routes()))
	t.Cleanup(ts.Close)
	return ts
}

func doGet(t *testing.T, ts *httptest.Server, path string) (int, []byte) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s 失败：%v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func doPost(t *testing.T, ts *httptest.Server, path, body string, withHeader bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if withHeader {
		req.Header.Set(reqHeader, "1")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败：%v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// ---------------------------------------------------------------------------

func TestStatusReportsIdentity(t *testing.T) {
	ts := newTestServer(t)
	code, body := doGet(t, ts, "/api/status")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", code, body)
	}
	var st statusResp
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if st.App != AppName {
		t.Errorf("app 字段应为 %q，实际 %q", AppName, st.App)
	}
	if st.Settings.Parallel < 1 {
		t.Errorf("并发数应有默认值，实际 %d", st.Settings.Parallel)
	}
	if st.Settings.CodecOrder == "" {
		t.Error("编码优先级不应为空")
	}
	if st.Running {
		t.Error("刚起来时不该是运行中")
	}
}

func TestStaticAssetsAllServed(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/", "/static/style.css", "/static/app.js", "/favicon.ico"} {
		code, body := doGet(t, ts, p)
		if code != http.StatusOK {
			t.Errorf("%s 期望 200，得到 %d", p, code)
			continue
		}
		if len(body) == 0 {
			t.Errorf("%s 内容为空", p)
		}
	}
}

func TestUnknownPathIs404(t *testing.T) {
	ts := newTestServer(t)
	if code, _ := doGet(t, ts, "/nope"); code != http.StatusNotFound {
		t.Errorf("未知路径期望 404，得到 %d", code)
	}
}

// 写操作必须带自定义头 —— 这是防跨站的唯一一道闸，不能退化成「有就查、没有也放行」。
func TestPostRequiresOriginHeader(t *testing.T) {
	ts := newTestServer(t)

	code, body := doPost(t, ts, "/api/config", `{"parallel":8}`, false)
	if code != http.StatusForbidden {
		t.Fatalf("缺头时期望 403，得到 %d：%s", code, body)
	}

	code, body = doPost(t, ts, "/api/config", `{"parallel":8}`, true)
	if code != http.StatusOK {
		t.Fatalf("带头时期望 200，得到 %d：%s", code, body)
	}
	var out struct {
		Settings settingsDTO `json:"settings"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if out.Settings.Parallel != 8 {
		t.Errorf("并发数应被写入 8，实际 %d", out.Settings.Parallel)
	}
}

func TestExtractShareText(t *testing.T) {
	ts := newTestServer(t)
	// 链接后面必须是分隔符，否则会把中文字也吃进 URL（这是提取器的既有约定）。
	text := "【标题】 https://www.bilibili.com/video/BV1GJ411x7h7/ 以及 BV1xx411c7mD"
	code, body := doPost(t, ts, "/api/extract", `{"text":`+jsonString(text)+`}`, true)
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", code, body)
	}
	var out struct {
		Count   int         `json:"count"`
		Targets []targetDTO `json:"targets"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if out.Count != 2 {
		t.Fatalf("期望识别 2 个地址，实际 %d：%s", out.Count, body)
	}
	if out.Targets[0].ID != "BV1GJ411x7h7" {
		t.Errorf("第 1 个地址应保持链接原样解析出 BV1GJ411x7h7，实际 %q", out.Targets[0].ID)
	}
}

func TestExtractRejectsNonBilibili(t *testing.T) {
	ts := newTestServer(t)
	code, body := doPost(t, ts, "/api/extract",
		`{"text":"看看这个 https://example.com/video/BV1GJ411x7h7"}`, true)
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", code, body)
	}
	var out struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Count != 0 {
		t.Errorf("非 B 站链接不该被当成地址，实际识别到 %d 个", out.Count)
	}
}

func TestTaskIdleSnapshotShape(t *testing.T) {
	ts := newTestServer(t)
	code, body := doGet(t, ts, "/api/task?since=0")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", code)
	}
	var dto taskDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if dto.State != "idle" {
		t.Errorf("初始状态应为 idle，实际 %q", dto.State)
	}
	// 四个阶段键必须齐 —— 前端直接按这些键名取节点，缺一个就会有格子永远不变色。
	for _, k := range []string{"parse", "video", "audio", "mux"} {
		if _, ok := dto.Steps[k]; !ok {
			t.Errorf("steps 缺少 %q 阶段", k)
		}
	}
	if dto.Running {
		t.Error("初始不该是运行中")
	}
}

func TestDownloadBeforeParseIsRejected(t *testing.T) {
	ts := newTestServer(t)
	code, body := doPost(t, ts, "/api/download", `{}`, true)
	if code != http.StatusBadRequest {
		t.Fatalf("未解析就下载应被拒，实际 %d：%s", code, body)
	}
	if !strings.Contains(string(body), "解析") {
		t.Errorf("错误信息应提示先解析，实际：%s", body)
	}
}

func TestLoginStatusIdle(t *testing.T) {
	ts := newTestServer(t)
	code, body := doGet(t, ts, "/api/login/status")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", code)
	}
	var dto loginDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if dto.State != "idle" {
		t.Errorf("初始登录状态应为 idle，实际 %q", dto.State)
	}
	if dto.HasQR {
		t.Error("尚未开始时不该有二维码")
	}
}

func TestLogLevelClassification(t *testing.T) {
	cases := map[string]string{
		"✓ 完成 x.mp4":   "ok",
		"✗ 解析失败":       "err",
		"取流失败（可能是未登录）": "warn",
		"正在封装":         "",
	}
	for line, want := range cases {
		if got := levelOf(line); got != want {
			t.Errorf("levelOf(%q) = %q，期望 %q", line, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 前端与页面的一致性
//
// JS 里 $("x") 引用了 HTML 中不存在的 id 时，运行时只会静默失效
// （拿不到元素 → 报错被 catch 吞掉 → 界面某块永远不动），肉眼极难发现。
// 这条测试把它变成编译期性质的错误。

var (
	reHTMLID = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	reJSID   = regexp.MustCompile(`\$\("([A-Za-z0-9_-]+)"\)`)
)

func TestFrontendIDsExistInHTML(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")

	ids := map[string]bool{}
	for _, m := range reHTMLID.FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}

	var missing []string
	for _, m := range reJSID.FindAllStringSubmatch(js, -1) {
		if !ids[m[1]] {
			missing = append(missing, m[1])
		}
	}

	// 拼接出来的 id 正则发现不了，按模板逐个列出来核对。
	for _, mode := range []string{"web", "tv"} {
		for _, pat := range []string{
			"panel-%s", "qr-img-%s", "qr-mask-%s", "qr-state-%s",
			"qr-remain-%s", "btn-qr-%s-start", "btn-qr-%s-cancel",
		} {
			id := fmt.Sprintf(pat, mode)
			if !ids[id] {
				missing = append(missing, id)
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("app.js 引用了 index.html 里不存在的 id：%v", missing)
	}
}

func TestFrontendUsesOriginHeaderOnWrites(t *testing.T) {
	js := readAsset(t, "app.js")
	// api() 是唯一的出口，它必须把自定义头带上 —— 否则所有写操作都会被 403。
	if !strings.Contains(js, `"X-BBDown-Native": "1"`) {
		t.Fatal("app.js 里没有带上 X-BBDown-Native 请求头")
	}
	if !strings.Contains(js, "headers: HEADERS") {
		t.Fatal("app.js 的 api() 没有把 HEADERS 用在 fetch 上")
	}
}

func TestStyleSheetHasBothThemes(t *testing.T) {
	css := readAsset(t, "style.css")
	if !strings.Contains(css, ":root{") {
		t.Error("样式表缺少亮色主题变量")
	}
	if !strings.Contains(css, `[data-theme="dark"]`) {
		t.Error("样式表缺少暗色主题变量")
	}
}

// ---------------------------------------------------------------------------
// 补充登录通道（浏览器 / 凭证文件 / 账号密码）
//
// 这些用例全部做成离线的：它们只覆盖「参数校验」与「本地读取」这两段，
// 任何会真的去请求 B 站的路径都不在这里跑。

func TestLoginBrowsersReturnsArray(t *testing.T) {
	ts := newTestServer(t)
	code, body := doGet(t, ts, "/api/login/browsers")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", code, body)
	}
	var dto struct {
		OK       bool `json:"ok"`
		Profiles []struct {
			Index   int    `json:"index"`
			Browser string `json:"browser"`
			Name    string `json:"name"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("返回不是合法 JSON：%v（%s）", err, body)
	}
	if !dto.OK {
		t.Errorf("ok 应当为 true：%s", body)
	}
	// profiles 必须是数组而不是 null —— 前端会直接 .map() 它。
	if !strings.Contains(string(body), `"profiles":[`) {
		t.Errorf("profiles 应当是数组：%s", body)
	}
}

func TestLoginImportRejectsGarbageText(t *testing.T) {
	ts := newTestServer(t)
	code, body := doPost(t, ts, "/api/login/import",
		`{"text":"这是一段完全无关的文字，没有凭据。"}`, true)
	if code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d：%s", code, body)
	}
	if !strings.Contains(string(body), "凭证") {
		t.Errorf("报错应当说明没找到凭证：%s", body)
	}
}

func TestLoginImportNeedsTextOrPath(t *testing.T) {
	ts := newTestServer(t)
	code, body := doPost(t, ts, "/api/login/import", `{}`, true)
	if code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d：%s", code, body)
	}
	if !strings.Contains(string(body), "粘贴") {
		t.Errorf("报错应当提示粘贴或选文件：%s", body)
	}
}

func TestLoginImportMissingFile(t *testing.T) {
	ts := newTestServer(t)
	// 注意 jsonString 已经带上两端的引号，这里不能再包一层。
	body := `{"path":` + jsonString(filepath.Join(t.TempDir(), "不存在.data")) + `}`
	code, raw := doPost(t, ts, "/api/login/import", body, true)
	if code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d：%s", code, raw)
	}
	if !strings.Contains(string(raw), "读取文件失败") {
		t.Errorf("报错应当说明读文件失败：%s", raw)
	}
}

// 只有 access_token（BBDownTV.data）时要走单项导入，不去校验 cookie。
func TestLoginImportTokenOnly(t *testing.T) {
	ts := newTestServer(t)
	const tok = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH"
	code, body := doPost(t, ts, "/api/login/import",
		`{"text":"access_token=`+tok+`"}`, true)
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", code, body)
	}
	if !strings.Contains(string(body), `"kind":"token"`) {
		t.Errorf("应当报告导入的是 token：%s", body)
	}
	if strings.Contains(string(body), tok) {
		t.Error("响应里不该回显 token 本身")
	}
}

func TestLoginPasswordValidatesInput(t *testing.T) {
	ts := newTestServer(t)
	// 空账号必须在打网络之前就被拦下。
	code, body := doPost(t, ts, "/api/login/password", `{"username":"","password":"x"}`, true)
	if code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d：%s", code, body)
	}
	if !strings.Contains(string(body), "账号") {
		t.Errorf("报错应当提示填账号：%s", body)
	}
}

func TestLoginFromBrowserRejectsBadIndex(t *testing.T) {
	ts := newTestServer(t)
	code, body := doPost(t, ts, "/api/login/from-browser", `{"index":9999}`, true)
	if code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d：%s", code, body)
	}
	if !strings.Contains(string(body), "浏览器") {
		t.Errorf("报错应当提到浏览器：%s", body)
	}
}

// 新增的写接口同样受 CSRF 头保护 —— 漏一个就等于开了个后门。
func TestLoginExtrasRequireCSRFHeader(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{
		"/api/login/import",
		"/api/login/from-browser",
		"/api/login/password",
	} {
		code, body := doPost(t, ts, path, `{}`, false)
		if code != http.StatusForbidden {
			t.Errorf("%s 不带来源标记应当 403，实际 %d：%s", path, code, body)
		}
	}
}

// ---------------------------------------------------------------------------

func readAsset(t *testing.T, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(staticFS, name)
	if err != nil {
		t.Fatalf("读不到内嵌资源 %s：%v", name, err)
	}
	return string(raw)
}

// jsonString 把 Go 字符串转成可以塞进 JSON 字面量的形式。
func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

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
	"time"

	"bbdown-native/internal/app"
	"bbdown-native/internal/bilibili"
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

// TestHoldKeepsServerAlive 锁住常驻模式：原生窗口拉起的浏览器服务
// 即使浏览器关掉（byeAt 设置）且空闲很久，也不能自动收摊。
// regressions：不加 Hold 时，用户关掉刷视频浏览器页面 30 秒后服务退出，
// 回到原生窗口再点「刷视频」就打不开了。
func TestHoldKeepsServerAlive(t *testing.T) {
	s := &Server{
		seen:     true,
		lastSeen: time.Now().Add(-24 * time.Hour), // 远超 idleTimeout
		byeAt:    time.Now().Add(-24 * time.Hour), // 远超 byeGrace
	}
	if !s.shouldQuit() {
		t.Fatal("前置条件：非常驻时这种状态应该判定为可退出")
	}
	s.Hold()
	if s.shouldQuit() {
		t.Fatal("Hold 之后即使浏览器关闭+长时间空闲也不该退出")
	}
	// 重复 Hold 幂等
	s.Hold()
	if s.shouldQuit() {
		t.Fatal("重复 Hold 不该改变常驻语义")
	}
}

// TestEmbeddedPageAutoOpen 锁住原生内嵌页的自动展开钩子：原生窗口导航到
// ?embed=feed / ?embed=login 后，页面必须自己展开对应视图，否则用户看到的是
// 「页签高亮刷视频/登录、内容却是下载页」。
func TestEmbeddedPageAutoOpen(t *testing.T) {
	js, err := fs.ReadFile(staticFS, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	if !strings.Contains(s, `embedView === "feed"`) || !strings.Contains(s, "feedOpen()") {
		t.Fatal("app.js 缺少 embed=feed 自动展开")
	}
	// 内嵌页签里那个「✕ 关闭」必须收起来：点了只会露出网页版下载界面，
	// 用户会以为刷视频页崩了（切页本来就走原生页签条）。函数本身也要挡住。
	if !strings.Contains(s, `classList.add("embed-feed")`) {
		t.Fatal("app.js 缺少 embed=feed 标记，关闭键收不起来")
	}
	if !strings.Contains(s, `contains("embed-feed")`) {
		t.Fatal("feedClose 在内嵌模式下必须直接返回")
	}
	css, err := fs.ReadFile(staticFS, "style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "body.embed-feed #feed-close") {
		t.Fatal("style.css 缺少收起 #feed-close 的规则")
	}
	if !strings.Contains(s, `embedView === "login"`) || !strings.Contains(s, `classList.add("embed-login")`) {
		t.Fatal("app.js 缺少 embed=login 自动展开")
	}
	// 原生账号卡的「浏览器导入 / 凭证文件 / 账号密码」都要能在内嵌页里用，
	// 对应面板与提交按钮必须存在。
	for _, id := range []string{
		`id="panel-browser"`, `id="panel-file"`, `id="panel-password"`,
		`id="btn-import-browser"`, `id="btn-import-cred"`, `id="btn-login-password"`,
	} {
		html, err := fs.ReadFile(staticFS, "index.html")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(html), id) {
			t.Errorf("index.html 缺少 %s", id)
		}
	}
}

// TestFeedPreviewStreamsProgressively 锁住「边下边播」：预览必须用
// streamIntoBuffer 边收边 append，缓冲到 PREVIEW_START_SEC 就开播。
// 旧实现先 fetchChunks 把整条流下完再统一 append，4K 视频要干等 40 秒
// 才出第一帧（用户反馈的「加载视频很慢」就是它）。
//
// 播放核心已抽成 playerOpen/playerPrefetch（精选页与首页播放层共用），
// 所以这里断言的是核心里的几个关键动作，而不是某个具体面板的函数名。
func TestFeedPreviewStreamsProgressively(t *testing.T) {
	js, err := fs.ReadFile(staticFS, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	if strings.Contains(s, "fetchChunks") {
		t.Fatal("app.js 里还有整段下载的 fetchChunks，预览会退化成等整条流下完")
	}
	// 开播判定必须基于缓冲量，而不是「两条流都下载完」；
	// 预取也必须在（换下一个不用再等一次解析）。
	for _, want := range []string{
		"async function playerOpen(",
		"playerPrefetch(",
		"streamIntoBuffer(",
		"PREVIEW_START_SEC",
		"waitAhead(",
		"streamIntoBuffer(vb,",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js 缺少 %s（边下边播/背压/预取）", want)
		}
	}
}

// TestFeedListCache 锁住热门榜短缓存：反复打开刷视频面板不该每次重打 B 站。
func TestFeedListCache(t *testing.T) {
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")
	items := []bilibili.FeedItem{{Bvid: "BV1xx411c7mD", Title: "第一条"}}

	if _, ok := s.cachedFeed("popular", 1); ok {
		t.Fatal("空缓存不应命中")
	}
	s.cacheFeed("popular", 1, items)
	got, ok := s.cachedFeed("popular", 1)
	if !ok || len(got) != 1 || got[0].Bvid != items[0].Bvid {
		t.Fatalf("缓存没命中：ok=%v items=%v", ok, got)
	}
	// 过期就不该再给
	s.feedMu.Lock()
	e := s.feedCache["popular:1"]
	e.at = time.Now().Add(-2 * feedCacheTTL)
	s.feedCache["popular:1"] = e
	s.feedMu.Unlock()
	if _, ok := s.cachedFeed("popular", 1); ok {
		t.Fatal("过期缓存不应命中")
	}
	// 个性化推荐不缓存：内容要新鲜
	s.cacheFeed("rcmd", 0, items)
	if _, ok := s.cachedFeed("rcmd", 0); ok {
		t.Fatal("个性化推荐不该走缓存")
	}
}

// TestPreviewFlightDedupes 锁住预览的并发合并与短缓存：列表自动选中和用户
// 点卡片经常同时发同一个 bvid，早先会打两遍 B 站（各 1~5 秒）。
func TestPreviewFlightDedupes(t *testing.T) {
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")
	const key = "bv1xx411c7md#1"

	first, owner := s.beginPreview(key)
	if !owner {
		t.Fatal("第一个请求应当是解析方")
	}
	second, owner2 := s.beginPreview(key)
	if owner2 {
		t.Fatal("解析过程中的第二个请求不该再打一遍 B 站")
	}
	if second != first {
		t.Fatal("并发请求应当共享同一次解析")
	}

	go s.finishPreview(key, first, previewDTO{Bvid: "BV1xx411c7mD"}, 0, nil)
	select {
	case <-second.done:
	case <-time.After(time.Second):
		t.Fatal("等待方没有被唤醒")
	}
	if second.err != nil || second.dto.Bvid != "BV1xx411c7mD" {
		t.Fatalf("等待方拿到的结果不对：%+v err=%v", second.dto, second.err)
	}

	// 完成后再来一次：命中缓存，不再解析
	cached, owner3 := s.beginPreview(key)
	if owner3 || cached != first {
		t.Fatal("5 分钟内重复请求应当命中缓存")
	}

	// 失败不进缓存，下次要重新解析
	badKey := "bv1xx411c7me#1"
	call, _ := s.beginPreview(badKey)
	s.finishPreview(badKey, call, previewDTO{}, http.StatusBadGateway, fmt.Errorf("网络炸了"))
	if _, owner := s.beginPreview(badKey); !owner {
		t.Fatal("失败的结果不该缓存在 map 里")
	}
}

// TestEmbeddedQuickDownloadLeavesPage 锁住内嵌页里「一键下载」的新语义：
// 点了就交给原生窗口切到「下载」页签，页面自己要把播放停掉（否则页签切走了
// 声音还在响），并且不再提示「后台下载」那种让人以为没反应的话。
func TestEmbeddedQuickDownloadLeavesPage(t *testing.T) {
	s := readAsset(t, "app.js")
	start := strings.Index(s, "async function quickDownload")
	if start < 0 {
		t.Fatal("app.js 找不到 quickDownload")
	}
	rest := s[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("quickDownload 函数体没有正常结束")
	}
	body := rest[:end]
	if !strings.Contains(body, "closePlayLayer()") {
		t.Error("内嵌页点一键下载后要停掉自己的播放：窗口马上切去下载页签")
	}
	if strings.Contains(body, "后台") {
		t.Error("内嵌页还在提示「后台下载」：现在会直接跳到下载页签")
	}
	if !strings.Contains(body, "closeOverlays()") {
		t.Error("浏览器方式仍要收起浮层、滚回任务区")
	}
}

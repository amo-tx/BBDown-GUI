// Package app 把接口层、下载层、封装层串成「一次完整的下载」，
// 并负责配置文件的读写。界面只跟这一层打交道。
package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bbdown-native/internal/bilibili"
)

// 默认值。
const (
	// DefaultCodecOrder 是编码偏好，靠前的优先。
	//
	// 默认给 AVC 优先，不是 HEVC —— 这是被用户实测逼出来的决定：
	// HEVC 体积小，同画质下比 AVC 省三成左右，看着很美好，但**兼容性代价很大**。
	// 用户实测「哔哩哔哩客户端打开全黑，VLC / PotPlayer 等其他播放器正常」。
	//文件本身完全健康（全片解码零错误、逐包比对零差异、hvcC 参数集齐全），
	//	是 B 站客户端自己的解码路径吃不下这个 HEVC 码流。
	//AVC(H.264) 是 Win11 自带解码器、绝大多数播放器与编辑软件都支持的，
	//	拿一点体积换「到哪都能播」，这个交易对下载器来说是划算的。
	//	仍想要小体积的话，界面「编码」分段里选 HEVC 或 AV1 即可。
	DefaultCodecOrder = "avc,hevc,av1"
	ConfigName        = "config.json"

	// 主题。原生窗口用这两个值记用户上次的选择。
	ThemeLight = "light"
	ThemeDark  = "dark"
)

// Config 是落盘的配置。字段名用 snake_case，方便用户手改。
type Config struct {
	// Cookie 是网页端凭证（SESSDATA 等），决定能不能看高清。
	Cookie string `json:"cookie"`
	// AccessToken 是电视端凭证，比 cookie 活得久（约 180 天）。
	AccessToken string `json:"access_token"`

	// UName / MID 只为界面展示，不参与鉴权。
	UName string `json:"uname"`
	MID   int64  `json:"mid"`

	DownloadDir string `json:"download_dir"`

	// Quality 是期望画质编号；0 表示"能拿到多高就多高"。
	Quality int `json:"quality"`
	// CodecOrder 是编码偏好，逗号分隔，靠前的优先。
	CodecOrder string `json:"codec_order"`
	// Parallel 是单文件的并发连接数。
	Parallel int `json:"parallel"`
	// KeepTemp 为真时保留中间的 .m4s 与 .bbdl，便于排查。
	KeepTemp bool `json:"keep_temp"`

	// Danmaku 决定弹幕怎么存：ass / xml / both / off。
	// 默认 ass —— 播放器会自动加载与视频同名的 .ass，只有它才「看得见效果」。
	Danmaku string `json:"danmaku"`
	// Subtitle 决定字幕怎么存：srt / json / both / off。
	// 默认 srt —— 通用格式，所有播放器和剪辑软件都认。
	Subtitle string `json:"subtitle"`
	// SaveCover 决定要不要把封面图存到本地。
	SaveCover bool `json:"save_cover"`

	// Theme 记用户选的界面主题：light / dark。空值按浅色处理。
	Theme string `json:"theme"`

	// path 是配置文件的绝对路径，不落盘。
	path string `json:"-"`
}

// IsDark 判断当前是不是深色主题。
func (c *Config) IsDark() bool { return c.Theme == ThemeDark }

// 附加内容的存法枚举。
const (
	ExtraOff  = "off"
	ExtraBoth = "both"

	DanmakuASS   = "ass"
	DanmakuXML   = "xml"
	SubtitleSRT  = "srt"
	SubtitleJSON = "json"
)

// DefaultConfig 返回一份开箱可用的配置。
func DefaultConfig() *Config {
	return &Config{
		DownloadDir: defaultDownloadDir(),
		CodecOrder:  DefaultCodecOrder,
		Parallel:    6,
		Danmaku:     DanmakuASS,
		Subtitle:    SubtitleSRT,
		SaveCover:   true,
	}
}

// WantsDanmaku 判断要不要下弹幕。
func (c *Config) WantsDanmaku() bool { return c.Danmaku != "" && c.Danmaku != ExtraOff }

// WantsSubtitle 判断要不要下字幕。
func (c *Config) WantsSubtitle() bool { return c.Subtitle != "" && c.Subtitle != ExtraOff }

// normalizeExtra 把一个枚举字段收敛到合法取值，非法就退回默认值。
func normalizeExtra(v, def string, allowed ...string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || v == ExtraOff {
		return ExtraOff
	}
	for _, a := range allowed {
		if v == a || v == ExtraBoth {
			return v
		}
	}
	return def
}

// defaultDownloadDir 把下载目录放在 exe 同级的 downloads 下。
//
// 刻意不用「视频」库：这是个绿色整合包，所有东西都待在自己的目录里，
// 用户拷走整个文件夹就能带走全部数据。
func defaultDownloadDir() string {
	dir, err := exeDir()
	if err != nil {
		if home, e := os.UserHomeDir(); e == nil {
			return filepath.Join(home, "BBDown")
		}
		return "downloads"
	}
	return filepath.Join(dir, "downloads")
}

// exeDir 返回可执行文件所在目录。
func exeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// ConfigPath 返回配置文件的绝对路径（与 exe 同级）。
func ConfigPath() (string, error) {
	dir, err := exeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigName), nil
}

// LoadConfig 读取配置。文件不存在时返回默认配置。
//
// 任何解析失败都退回默认值并把原因返回给调用方 —— 一个坏掉的 config.json
// 不该让程序起不来。
func LoadConfig() (*Config, string) {
	path, err := ConfigPath()
	if err != nil {
		c := DefaultConfig()
		return c, ""
	}
	return LoadConfigFrom(path)
}

// LoadConfigFrom 从指定路径读配置，便于测试。
func LoadConfigFrom(path string) (*Config, string) {
	cfg := DefaultConfig()
	cfg.path = path

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, ""
		}
		return cfg, fmt.Sprintf("读取配置失败（已用默认值）：%v", err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, ""
	}
	// 有些编辑器（旧版 PowerShell、Windows 记事本）会在文件头写 UTF-8 BOM
	// （EF BB BF）。标准 json 解析器不认 BOM，会报"invalid character '\\xef'
	// looking for beginning of value"，直接把整份配置判坏、退回默认。这里把
	// BOM 剥掉再解析，免得用户只是手改了个主题就被当成坏文件。
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if err := json.Unmarshal(raw, cfg); err != nil {
		c := DefaultConfig()
		c.path = path
		return c, fmt.Sprintf("配置文件格式不对（已用默认值）：%v", err)
	}

	cfg.normalize()
	return cfg, ""
}

// normalize 补齐不合理或缺失的字段。
func (c *Config) normalize() {
	if strings.TrimSpace(c.DownloadDir) == "" {
		c.DownloadDir = defaultDownloadDir()
	}
	if strings.TrimSpace(c.CodecOrder) == "" {
		c.CodecOrder = DefaultCodecOrder
	} else if isLegacyDefaultCodecOrder(c.CodecOrder) {
		// 老版本把 HEVC排在了首位，现在改回 AVC 优先。
		// 只迁移「恰好等于旧默认值」这一种情况 —— 用户要是自己动手调过顺序，
		// 或者只填了 HEVC（明确表达偏好），都不该被我们覆盖。
		c.CodecOrder = DefaultCodecOrder
	}
	if c.Parallel < 1 {
		c.Parallel = 6
	}
	if c.Parallel > 32 {
		c.Parallel = 32
	}
	c.Cookie = bilibili.NormalizeCookie(c.Cookie)
	c.AccessToken = strings.TrimSpace(c.AccessToken)

	// 主题只认这两个值，别的一律当浅色 —— 免得手改配置文件时写错一个
	// 字符串就让整个界面变成没配色的一堆黑块。
	if c.Theme != ThemeDark {
		c.Theme = ThemeLight
	}

	// 三个附加内容开关：文件里没写（老版本留下的配置）时也要保持默认开启，
	// 所以这里不能用「零值即关闭」来判断 —— 只有显式写成 off 才关。
	c.Danmaku = normalizeExtra(c.Danmaku, DanmakuASS, DanmakuASS, DanmakuXML)
	c.Subtitle = normalizeExtra(c.Subtitle, SubtitleSRT, SubtitleSRT, SubtitleJSON)
}

// Save 写回配置文件（与 exe 同级）。
func (c *Config) Save() error {
	if c.path == "" {
		p, err := ConfigPath()
		if err != nil {
			return err
		}
		c.path = p
	}
	return c.SaveTo(c.path)
}

// SaveTo 写到指定路径，先写临时文件再改名，避免中途断电留下半个文件。
func (c *Config) SaveTo(path string) error {
	c.normalize()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	// 配置文件里有 SESSDATA，权限收窄到仅当前用户。
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = err // Windows 上 chmod 基本是空操作，忽略
	}
	return os.Rename(tmp, path)
}

// Path 返回当前配置的落盘位置（可能为空）。
func (c *Config) Path() string { return c.path }

// isLegacyDefaultCodecOrder 判断配置里的编码顺序是否恰好是旧版默认值
// "hevc,avc,av1"（忽略大小写与空白）。
//
// 这样判定是为了「只动没被用户改过的值」：顺序完全一致说明这是当年自动
// 写进去的默认值，而不是用户的选择。仅含 HEVC、或顺序不同，都是有意为之。
func isLegacyDefaultCodecOrder(s string) bool {
	var got []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			got = append(got, strings.ToLower(p))
		}
	}
	want := strings.Split("hevc,avc,av1", ",")
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// CodecList 把 CodecOrder 拆成列表。
func (c *Config) CodecList() []string {
	var out []string
	for _, p := range strings.Split(c.CodecOrder, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return strings.Split(DefaultCodecOrder, ",")
	}
	return out
}

// QualityPriority 返回按优先级排列的画质编号。
//
// Quality 为 0 时返回空列表 —— 交给 SelectVideo 走"能拿多高拿多高"。
// 否则把选中的画质排在最前，其余按从高到低兜底，这样即使账号权限
// 不够，也能顺位降级而不是直接失败。
func (c *Config) QualityPriority() []int {
	all := []int{127, 126, 125, 120, 116, 112, 80, 64, 32, 16, 6}
	if c.Quality == 0 {
		return nil
	}
	out := []int{c.Quality}
	for _, q := range all {
		if q != c.Quality {
			out = append(out, q)
		}
	}
	return out
}

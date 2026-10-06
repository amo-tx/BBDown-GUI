// Package app 把接口层、下载层、封装层串成「一次完整的下载」，
// 并负责配置文件的读写。界面只跟这一层打交道。
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bbdown-native/internal/bilibili"
)

// 默认值。
const (
	DefaultCodecOrder = "hevc,avc,av1"
	ConfigName        = "config.json"
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

	// path 是配置文件的绝对路径，不落盘。
	path string `json:"-"`
}

// DefaultConfig 返回一份开箱可用的配置。
func DefaultConfig() *Config {
	return &Config{
		DownloadDir: defaultDownloadDir(),
		CodecOrder:  DefaultCodecOrder,
		Parallel:    6,
	}
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
	}
	if c.Parallel < 1 {
		c.Parallel = 6
	}
	if c.Parallel > 32 {
		c.Parallel = 32
	}
	c.Cookie = bilibili.NormalizeCookie(c.Cookie)
	c.AccessToken = strings.TrimSpace(c.AccessToken)
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

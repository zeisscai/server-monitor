// Package config 负责配置文件的生成、读取、校验与持久化。
//
// 配置文件为 JSON（默认 /opt/server-monitor/config.json），文件权限 0600，
// 因为其中保存了 Web 登录账号明文密码，便于部署后查询。
// 二进制、配置文件与数据目录统一位于默认安装目录 /opt/server-monitor/ 下。
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 默认值：程序二进制、配置文件、数据目录统一位于默认安装目录下。
const (
	DefaultInstallDir     = "/opt/server-monitor"
	DefaultConfigPath     = DefaultInstallDir + "/config.json"
	DefaultDataDir        = DefaultInstallDir + "/data"
	DefaultWebPort        = 8080
	DefaultReceivePort    = 9100
	DefaultReportInterval = 5
	DefaultServerName     = "server"
)

// AllowedIntervals 上报频率（秒）仅允许这三个取值，同时决定采集频率。
var AllowedIntervals = []int{2, 5, 10}

// IntervalWhenReportOff 上报功能关闭时的默认采集频率（秒）。
const IntervalWhenReportOff = 5

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Config 是配置文件的完整结构。
type Config struct {
	// 基础
	UUID          string `json:"uuid"`           // 本机识别码，首次启动自动生成，只读
	ServerName    string `json:"server_name"`    // 本机在面板中的显示名称
	WebPort       int    `json:"web_port"`       // Web 界面服务端口
	WebUser       string `json:"web_user"`       // Web 登录账号
	WebPass       string `json:"web_pass"`       // Web 登录密码（明文，便于查询）
	SessionSecret string `json:"session_secret"` // 会话签名密钥，自动生成
	DataDir       string `json:"data_dir"`       // 数据目录（SQLite 存放位置）

	// 数据接收
	ReceiveEnabled bool   `json:"receive_enabled"` // 默认关闭
	ReceivePort    int    `json:"receive_port"`
	ReceiveAPIKey  string `json:"receive_api_key"` // 18 位，打开接收功能时自动生成，只读

	// 数据上报
	ReportEnabled    bool   `json:"report_enabled"`
	ReportTargetIP   string `json:"report_target_ip"`
	ReportTargetPort int    `json:"report_target_port"`
	ReportInterval   int    `json:"report_interval"` // 2 / 5 / 10
	ReportAPIKey     string `json:"report_api_key"`
}

// IsUUID 判断字符串是否为标准 8-4-4-4-12 的 UUID。
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

// NewUUID 生成一个 UUID v4（36 位，标准 8-4-4-4-12 格式）。
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("无法生成 UUID: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// apiKeyAlphabet 去掉了容易混淆的 0/O/1/l/I。
const apiKeyAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// NewAPIKey 生成一个 18 位的随机 API-Key。
func NewAPIKey() string {
	const n = 18
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("无法生成 API-Key: " + err.Error())
	}
	out := make([]byte, n)
	for i, v := range buf {
		out[i] = apiKeyAlphabet[int(v)%len(apiKeyAlphabet)]
	}
	return string(out)
}

// NewSecret 生成会话签名密钥。
func NewSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("无法生成密钥: " + err.Error())
	}
	return fmt.Sprintf("%x", buf)
}

// EffectiveInterval 返回实际生效的采集/上报间隔。
// 上报开启时使用配置的上报频率，否则固定 5 秒。
func (c Config) EffectiveInterval() time.Duration {
	if !c.ReportEnabled {
		return time.Duration(IntervalWhenReportOff) * time.Second
	}
	if !validInterval(c.ReportInterval) {
		return time.Duration(DefaultReportInterval) * time.Second
	}
	return time.Duration(c.ReportInterval) * time.Second
}

func validInterval(v int) bool {
	for _, a := range AllowedIntervals {
		if a == v {
			return true
		}
	}
	return false
}

// Normalize 补齐缺失项并纠正非法取值，返回被修正过的配置副本。
func (c Config) Normalize() Config {
	if !IsUUID(c.UUID) {
		c.UUID = NewUUID()
	}
	if strings.TrimSpace(c.ServerName) == "" {
		if hn, err := os.Hostname(); err == nil && strings.TrimSpace(hn) != "" {
			c.ServerName = hn
		} else {
			c.ServerName = DefaultServerName
		}
	}
	if c.WebPort <= 0 || c.WebPort > 65535 {
		c.WebPort = DefaultWebPort
	}
	if strings.TrimSpace(c.WebUser) == "" {
		c.WebUser = "smdadmin"
	}
	if c.SessionSecret == "" {
		c.SessionSecret = NewSecret()
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = DefaultDataDir
	}
	if c.ReceivePort <= 0 || c.ReceivePort > 65535 {
		c.ReceivePort = DefaultReceivePort
	}
	if !c.ReceiveEnabled {
		// 关闭状态下不保留 key，下次开启时重新生成。
		c.ReceiveAPIKey = ""
	} else if len(c.ReceiveAPIKey) != 18 {
		c.ReceiveAPIKey = NewAPIKey()
	}
	if !validInterval(c.ReportInterval) {
		c.ReportInterval = DefaultReportInterval
	}
	if c.ReportTargetPort < 0 || c.ReportTargetPort > 65535 {
		c.ReportTargetPort = 0
	}
	c.ReportTargetIP = strings.TrimSpace(c.ReportTargetIP)
	c.ServerName = strings.TrimSpace(c.ServerName)
	return c
}

// Validate 校验用户提交的配置是否合法。
func (c Config) Validate() error {
	if strings.TrimSpace(c.ServerName) == "" {
		return fmt.Errorf("服务器名称不能为空")
	}
	if len([]rune(c.ServerName)) > 64 {
		return fmt.Errorf("服务器名称过长（最多 64 个字符）")
	}
	if c.WebPort <= 0 || c.WebPort > 65535 {
		return fmt.Errorf("Web 端口必须在 1~65535 之间，当前为 %d", c.WebPort)
	}
	if c.ReceiveEnabled {
		if c.ReceivePort <= 0 || c.ReceivePort > 65535 {
			return fmt.Errorf("数据接收端口必须在 1~65535 之间，当前为 %d", c.ReceivePort)
		}
		if c.ReceivePort == c.WebPort {
			return fmt.Errorf("数据接收端口不能与 Web 端口相同（同为 %d）", c.WebPort)
		}
	}
	if c.ReportEnabled {
		if c.ReportTargetIP == "" {
			return fmt.Errorf("请填写上报目标服务器 IP")
		}
		if c.ReportTargetPort <= 0 || c.ReportTargetPort > 65535 {
			return fmt.Errorf("上报目标端口不合法")
		}
		if c.ReportTargetIP == "127.0.0.1" && c.ReportTargetPort == c.WebPort {
			return fmt.Errorf("上报目标端口指向了本机 Web 端口，请填写目标服务器的接收端口")
		}
		if !validInterval(c.ReportInterval) {
			return fmt.Errorf("上报频率仅支持 2 / 5 / 10 秒")
		}
		if strings.TrimSpace(c.ReportAPIKey) == "" {
			return fmt.Errorf("请填写上报 API-Key")
		}
	}
	return nil
}

// Manager 提供并发安全的配置读写。
type Manager struct {
	mu       sync.RWMutex
	path     string
	cfg      Config
	existed  bool
	onChange []func(Config)
}

// Load 读取配置文件；文件不存在时自动生成一份默认配置（含 UUID）。
func Load(path string) (*Manager, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	m := &Manager{path: abs}

	raw, err := os.ReadFile(abs)
	switch {
	case err == nil:
		m.existed = true
		var c Config
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("配置文件格式错误 %s: %w", abs, err)
		}
		m.cfg = c.Normalize()
	case os.IsNotExist(err):
		m.cfg = Config{}.Normalize()
	default:
		return nil, fmt.Errorf("读取配置文件失败 %s: %w", abs, err)
	}

	// 首次生成或补齐缺失字段后立即落盘，保证调用方随后一定能读到完整配置。
	if err := m.save(m.cfg); err != nil {
		return nil, err
	}
	return m, nil
}

// Path 返回配置文件绝对路径。
func (m *Manager) Path() string { return m.path }

// Existed 表示加载前配置文件是否已存在。
func (m *Manager) Existed() bool { return m.existed }

// Snapshot 返回当前配置的副本。
func (m *Manager) Snapshot() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Update 在副本上执行 fn，校验通过后持久化，最后在锁外通知变更回调。
func (m *Manager) Update(fn func(*Config)) (Config, error) {
	m.mu.Lock()
	next := m.cfg
	fn(&next)
	next = next.Normalize()
	if err := next.Validate(); err != nil {
		cur := m.cfg
		m.mu.Unlock()
		return cur, err
	}
	if err := m.save(next); err != nil {
		cur := m.cfg
		m.mu.Unlock()
		return cur, err
	}
	m.cfg = next
	// 复制回调列表后在锁外执行，回调内部会再次读取配置，避免重入死锁。
	callbacks := make([]func(Config), len(m.onChange))
	copy(callbacks, m.onChange)
	m.mu.Unlock()

	for _, cb := range callbacks {
		cb(next)
	}
	return next, nil
}

// OnChange 注册配置变更回调（用于重启采集/接收循环）。
func (m *Manager) OnChange(fn func(Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

func (m *Manager) save(c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败 %s: %w", dir, err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入配置文件失败 %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("保存配置文件失败 %s: %w", m.path, err)
	}
	_ = os.Chmod(m.path, 0o600)
	return nil
}

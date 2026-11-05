package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"server-monitor/internal/config"
	"server-monitor/internal/store"
)

// newTestEngine 构造一个仅用于直接调用 http.Handler 的引擎。
//
// 刻意不调用 Apply：本文件只验证接收端鉴权逻辑，不需要真的监听端口或启动采集循环。
func newTestEngine(t *testing.T, tune func(*config.Config)) (*Engine, config.Config) {
	t.Helper()
	dir := t.TempDir()

	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if _, err := cfg.Update(func(c *config.Config) {
		c.WebUser = "admin"
		c.WebPass = "pw123456"
		if tune != nil {
			tune(c)
		}
	}); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return New(context.Background(), cfg, st), cfg.Snapshot()
}

func postReport(e *Engine, apiKey, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	rec := httptest.NewRecorder()
	e.handleReport(rec, req)
	return rec
}

const reportBody = `{"uuid":"cccccccc-3333-4333-8333-cccccccccccc","name":"远端节点","cpu":12.5,"mem":33.3}`

func TestReportAcceptsValidKey(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })
	if len(c.ReceiveAPIKey) != 18 {
		t.Fatalf("开启接收应生成 18 位 Key，实际 %q", c.ReceiveAPIKey)
	}

	rec := postReport(e, c.ReceiveAPIKey, reportBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("正确 Key 应返回 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}

	latest, err := e.st.Latest()
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(latest) != 1 || latest[0].ServerName != "远端节点" || latest[0].Source != store.SourceRemote {
		t.Fatalf("上报数据未正确落库: %+v", latest)
	}
}

// 长度相同但内容不同的 Key、以及长度不同的 Key，都必须被拒绝。
// 后者专门覆盖「先 SHA-256 再比较」这一改动：若退回直接比较明文，
// 长度差异会成为可利用的旁路信息。
func TestReportRejectsInvalidKeys(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })
	key := c.ReceiveAPIKey

	// '0' 不在 Key 的字符集内（该字符集刻意排除了 0/O/1/l/I），
	// 因此替换首字符后必定与原 Key 不同，且长度一致。
	sameLenDifferent := "0" + key[1:]

	cases := []struct {
		name string
		key  string
	}{
		{"空 Key", ""},
		{"无关系字符串", "wrong-key"},
		{"同长度不同内容", sameLenDifferent},
		{"少一位", key[:17]},
		{"多一位", key + "x"},
		{"前后带空格", " " + key + " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postReport(e, tc.key, reportBody)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("非法 Key 应返回 401，实际 %d（%s）", rec.Code, rec.Body.String())
			}
		})
	}

	// 全部失败尝试都不应写库。
	latest, err := e.st.Latest()
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(latest) != 0 {
		t.Fatalf("鉴权失败不应写库，实际 %+v", latest)
	}
}

// 大小写差异必须被拒绝：这要求比较是精确的，而不是做了某种归一化。
func TestReportKeyIsCaseSensitive(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })

	mutated := c.ReceiveAPIKey
	if mutated[0] == 'A' {
		mutated = "B" + mutated[1:]
	} else {
		mutated = "A" + mutated[1:]
	}
	if rec := postReport(e, mutated, reportBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("大小写不同的 Key 应返回 401，实际 %d", rec.Code)
	}
}

func TestReportAcceptsBearerToken(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })

	req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(reportBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.ReceiveAPIKey)
	rec := httptest.NewRecorder()
	e.handleReport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer 形式应被接受，实际 %d（%s）", rec.Code, rec.Body.String())
	}
}

// 接收功能关闭时必须拒绝，且不因 Key 为空而误放行。
func TestReportRejectedWhenReceiveDisabled(t *testing.T) {
	e, _ := newTestEngine(t, func(c *config.Config) {
		c.ReceiveEnabled = false
		c.ReceiveAPIKey = ""
	})

	rec := postReport(e, "", reportBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("接收未开启应返回 403，实际 %d", rec.Code)
	}
}

func TestReportRejectsNonPost(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })

	req := httptest.NewRequest(http.MethodGet, "/api/report", nil)
	req.Header.Set("X-API-Key", c.ReceiveAPIKey)
	rec := httptest.NewRecorder()
	e.handleReport(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", rec.Code)
	}
}

// 已通过鉴权但内容不合法时仍应拒绝，且不写库。
func TestReportValidatesPayload(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })

	cases := []struct {
		name string
		body string
		code int
	}{
		{"非法 UUID", `{"uuid":"not-a-uuid","cpu":1,"mem":1}`, http.StatusBadRequest},
		{"cpu 超范围", `{"uuid":"cccccccc-3333-4333-8333-cccccccccccc","cpu":120,"mem":1}`, http.StatusBadRequest},
		{"mem 为负", `{"uuid":"cccccccc-3333-4333-8333-cccccccccccc","cpu":1,"mem":-1}`, http.StatusBadRequest},
		{"请求体非 JSON", `not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postReport(e, c.ReceiveAPIKey, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("应返回 %d，实际 %d（%s）", tc.code, rec.Code, rec.Body.String())
			}
		})
	}
}

// 上报端时钟明显异常时应以接收端时间为准，避免污染时间轴。
func TestReportClampsBadTimestamp(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })

	body := `{"uuid":"cccccccc-3333-4333-8333-cccccccccccc","name":"n","cpu":1,"mem":2,"ts":1}`
	if rec := postReport(e, c.ReceiveAPIKey, body); rec.Code != http.StatusOK {
		t.Fatalf("应接受该请求，实际 %d", rec.Code)
	}

	latest, err := e.st.Latest()
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("应写入一条记录，实际 %d", len(latest))
	}
	if latest[0].CreatedAt <= 1 {
		t.Fatalf("异常时间戳应被替换为接收端时间，实际 %d", latest[0].CreatedAt)
	}
}

// ---------- 系统信息（发行版 / 内核 / 开机时刻） ----------

func getAPI(e *Engine, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	rec := httptest.NewRecorder()
	e.handleServersAPI(rec, req)
	return rec
}

const sysReportBody = `{"uuid":"dddddddd-4444-4444-8444-dddddddddddd","name":"带系统信息","cpu":5,"mem":6,` +
	`"sys":{"os":"Ubuntu 22.04.3 LTS","kernel":"5.15.0-91-generic","boot_at":%d,"uptime":123}}`

// 上报体带 sys 时应落到 server_info，并且只在带 sys 的那次才更新。
func TestReportStoresSysInfo(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) {
		c.ReceiveEnabled = true
		c.APIEnabled = true
	})
	bootAt := time.Now().Add(-26 * time.Hour).UnixMilli()

	body := fmt.Sprintf(sysReportBody, bootAt)
	if rec := postReport(e, c.ReceiveAPIKey, body); rec.Code != http.StatusOK {
		t.Fatalf("应接受该请求，实际 %d（%s）", rec.Code, rec.Body.String())
	}

	inf, err := e.st.Info("dddddddd-4444-4444-8444-dddddddddddd")
	if err != nil {
		t.Fatalf("查询系统信息失败: %v", err)
	}
	if inf.OS != "Ubuntu 22.04.3 LTS" || inf.Kernel != "5.15.0-91-generic" || inf.BootAt != bootAt {
		t.Fatalf("系统信息未正确落库: %+v", inf)
	}

	// 后续不带 sys 的上报（绝大多数）不能把已有信息抹掉。
	plain := `{"uuid":"dddddddd-4444-4444-8444-dddddddddddd","name":"带系统信息","cpu":6,"mem":7}`
	if rec := postReport(e, c.ReceiveAPIKey, plain); rec.Code != http.StatusOK {
		t.Fatalf("应接受该请求，实际 %d", rec.Code)
	}
	again, err := e.st.Info("dddddddd-4444-4444-8444-dddddddddddd")
	if err != nil {
		t.Fatalf("查询系统信息失败: %v", err)
	}
	if again.OS != inf.OS || again.Kernel != inf.Kernel || again.BootAt != inf.BootAt {
		t.Fatalf("缺少 sys 的上报不应修改系统信息: %+v -> %+v", inf, again)
	}
}

// 未来时间点的 boot_at 必须被丢弃，否则运行时长会显示成负数或「几十天后」。
func TestReportRejectsFutureBootAt(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.ReceiveEnabled = true })
	future := time.Now().Add(48 * time.Hour).UnixMilli()
	postReport(e, c.ReceiveAPIKey, fmt.Sprintf(`{"uuid":"dddddddd-4444-4444-8444-dddddddddddd","cpu":1,"mem":1,"sys":{"os":"X","boot_at":%d}}`, future))

	inf, err := e.st.Info("dddddddd-4444-4444-8444-dddddddddddd")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if inf.BootAt != 0 {
		t.Fatalf("未来时间点不应被接收，实际 %d", inf.BootAt)
	}
	if inf.OS != "X" {
		t.Fatalf("同一块里的其它字段仍应写入，实际 %q", inf.OS)
	}
}

// 查询 API 的四种基本情况：未开启、无 Key、错 Key、正确 Key。
func TestServersAPIAuth(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) {
		c.ReceiveEnabled = true
		c.APIEnabled = true
	})
	if len(c.APIKey) != 18 {
		t.Fatalf("开启查询 API 应生成 18 位 Key，实际 %q", c.APIKey)
	}
	if c.APIKey == c.ReceiveAPIKey {
		t.Fatal("查询 Key 必须与接收 Key 不同")
	}

	if rec := getAPI(e, "/api/servers", c.APIKey); rec.Code != http.StatusOK {
		t.Fatalf("正确 Key 应返回 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := getAPI(e, "/api/servers", c.ReceiveAPIKey); rec.Code != http.StatusUnauthorized {
		t.Fatalf("用接收 Key 查数据应被拒绝，实际 %d", rec.Code)
	}
	if rec := getAPI(e, "/api/servers", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 Key 应返回 401，实际 %d", rec.Code)
	}

	// 关闭开关后，即使拿着原来的 Key 也必须拒绝。
	if _, err := e.cfg.Update(func(c *config.Config) { c.APIEnabled = false }); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if rec := getAPI(e, "/api/servers", c.APIKey); rec.Code != http.StatusForbidden {
		t.Fatalf("API 关闭后应返回 403，实际 %d", rec.Code)
	}
}

func TestServersAPIReturnsAllServers(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) {
		c.ReceiveEnabled = true
		c.APIEnabled = true
	})
	bootAt := time.Now().Add(-3 * 24 * time.Hour).UnixMilli()
	if rec := postReport(e, c.ReceiveAPIKey, fmt.Sprintf(sysReportBody, bootAt)); rec.Code != http.StatusOK {
		t.Fatalf("上报失败: %d", rec.Code)
	}

	rec := getAPI(e, "/api/servers", c.APIKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	var got apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !got.OK || got.Count != 1 || len(got.Servers) != 1 {
		t.Fatalf("响应内容不正确: %+v", got)
	}
	s := got.Servers[0]
	if s.Name != "带系统信息" || s.OS != "Ubuntu 22.04.3 LTS" || s.Kernel != "5.15.0-91-generic" {
		t.Fatalf("服务器信息不完整: %+v", s)
	}
	if s.CPU != 5 || s.Mem != 6 || s.Source != store.SourceRemote {
		t.Fatalf("指标不正确: %+v", s)
	}
	if s.IsSelf {
		t.Fatal("外部上报的服务器不应标记为本机")
	}
	// 运行时长由开机时刻推算，允许几十秒误差。
	if s.Uptime < 3*24*3600-60 || s.Uptime > 3*24*3600+60 {
		t.Fatalf("运行时长推算不正确: %d", s.Uptime)
	}
	if got.Self != c.UUID || got.SelfName != c.ServerName {
		t.Fatalf("响应应包含本机标识: %+v", got)
	}
}

func TestServersAPIRejectsNonGet(t *testing.T) {
	e, c := newTestEngine(t, func(c *config.Config) { c.APIEnabled = true })
	req := httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader("{}"))
	req.Header.Set("X-API-Key", c.APIKey)
	rec := httptest.NewRecorder()
	e.handleServersAPI(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应返回 405，实际 %d", rec.Code)
	}
}

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"server-monitor/internal/config"
	"server-monitor/internal/engine"
	"server-monitor/internal/store"
)

type harness struct {
	cfg *config.Manager
	st  *store.Store
	srv *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if _, err := cfg.Update(func(c *config.Config) {
		c.WebUser = "admin"
		c.WebPass = "pw123456"
	}); err != nil {
		t.Fatalf("写入账号失败: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	eng := engine.New(ctx, cfg, st)
	eng.Apply()

	ws, err := New(cfg, st, eng)
	if err != nil {
		t.Fatalf("构建 Web 服务失败: %v", err)
	}
	srv := httptest.NewServer(ws.Handler())
	t.Cleanup(srv.Close)

	return &harness{cfg: cfg, st: st, srv: srv}
}

func loginClient(t *testing.T, baseURL, user, pass string) (*http.Client, *http.Response) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 cookie jar 失败: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := client.PostForm(baseURL+"/login", url.Values{"username": {user}, "password": {pass}})
	if err != nil {
		t.Fatalf("登录请求失败: %v", err)
	}
	return client, resp
}

func TestAuthFlow(t *testing.T) {
	h := newHarness(t)

	// 未登录访问面板应跳转到登录页
	noRedirect := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noRedirect.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("未登录应返回 302，实际 %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/login") {
		t.Fatalf("未登录应跳转登录页，实际 %s", loc)
	}

	// 未登录访问接口应同样被拦截
	for _, path := range []string{"/api/overview", "/settings", "/server/x"} {
		r, err := noRedirect.Get(h.srv.URL + path)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusFound {
			t.Fatalf("%s 未登录应返回 302，实际 %d", path, r.StatusCode)
		}
	}

	// 错误密码
	_, resp = loginClient(t, h.srv.URL, "admin", "wrong-password")
	body := readBody(t, resp)
	if !strings.Contains(resp.Request.URL.RawQuery, "error") {
		t.Fatalf("错误密码应跳回带错误提示的登录页，实际 %s", resp.Request.URL.String())
	}
	if strings.Contains(body, "服务器面板") {
		t.Fatal("错误密码不应进入面板")
	}

	// 正确密码
	client, resp := loginClient(t, h.srv.URL, "admin", "pw123456")
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录后应返回 200，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "服务器面板") {
		t.Fatal("登录后应直接进入面板界面")
	}

	// 带登录态访问接口
	r, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("登录后接口应返回 200，实际 %d", r.StatusCode)
	}

	// 退出登录后失效
	lr, err := client.Get(h.srv.URL + "/logout")
	if err != nil {
		t.Fatalf("退出失败: %v", err)
	}
	lr.Body.Close()
	r2, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer r2.Body.Close()
	if !strings.HasSuffix(r2.Request.URL.Path, "/login") {
		t.Fatalf("退出后应跳转登录页，实际 %s", r2.Request.URL.Path)
	}
}

func TestSettingsToggleAndValidation(t *testing.T) {
	h := newHarness(t)
	client, resp := loginClient(t, h.srv.URL, "admin", "pw123456")
	readBody(t, resp)

	port := freePort(t)

	// 开启数据接收
	r, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"server_name":     {"测试节点"},
		"receive_enabled": {"1"},
		"receive_port":    {fmt.Sprint(port)},
	})
	if err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}
	readBody(t, r)
	if !strings.Contains(r.Request.URL.String(), "saved=1") {
		t.Fatalf("保存后应跳回设置页，实际 %s", r.Request.URL.String())
	}

	c := h.cfg.Snapshot()
	if !c.ReceiveEnabled || c.ReceivePort != port {
		t.Fatalf("接收配置未生效: %+v", c)
	}
	if len(c.ReceiveAPIKey) != 18 {
		t.Fatalf("开启接收应自动生成 18 位 Key，实际 %q", c.ReceiveAPIKey)
	}
	if c.ServerName != "测试节点" {
		t.Fatalf("服务器名称未更新: %q", c.ServerName)
	}

	// 设置页应展示 Key 与数据库占用
	page, err := client.Get(h.srv.URL + "/settings?saved=1")
	if err != nil {
		t.Fatalf("打开设置页失败: %v", err)
	}
	html := readBody(t, page)
	if !strings.Contains(html, c.ReceiveAPIKey) {
		t.Fatal("设置页未展示接收 API-Key")
	}
	if !strings.Contains(html, "数据库占用") || strings.Contains(html, `value="—"`) {
		t.Fatal("开启接收后设置页应显示数据库占用")
	}

	// 非法配置（开启上报但未填目标 IP）应被拒绝且不落盘
	bad, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"server_name":        {"测试节点"},
		"report_enabled":     {"1"},
		"report_target_ip":   {""},
		"report_target_port": {"9100"},
		"report_interval":    {"5"},
		"report_api_key":     {"abcdefghijklmnopqr"},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	badHTML := readBody(t, bad)
	if !strings.Contains(badHTML, "请填写上报目标服务器 IP") {
		t.Fatal("非法配置应给出校验提示")
	}
	if h.cfg.Snapshot().ReportEnabled {
		t.Fatal("非法配置不应生效")
	}

	// 非 2/5/10 的上报频率会被纠正为默认 5 秒
	okResp, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"server_name":        {"测试节点"},
		"report_enabled":     {"1"},
		"report_target_ip":   {"10.0.0.9"},
		"report_target_port": {"9100"},
		"report_interval":    {"7"},
		"report_api_key":     {"abcdefghijklmnopqr"},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, okResp)
	c = h.cfg.Snapshot()
	if !c.ReportEnabled || c.ReportInterval != config.DefaultReportInterval {
		t.Fatalf("非法频率应回落为默认值: %+v", c)
	}

	// 关闭接收：Key 清空、占用信息隐藏
	off, err := client.PostForm(h.srv.URL+"/settings", url.Values{"server_name": {"测试节点"}})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, off)
	c = h.cfg.Snapshot()
	if c.ReceiveEnabled || c.ReceiveAPIKey != "" {
		t.Fatalf("关闭接收后应清空 Key: %+v", c)
	}
	page2, _ := client.Get(h.srv.URL + "/settings")
	html2 := readBody(t, page2)
	if !strings.Contains(html2, `value="—"`) {
		t.Fatal("关闭接收后不应显示数据库占用")
	}

	// engine 的监听应已停止
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.cfg.Snapshot().ReceiveAPIKey == "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReportRoundTrip(t *testing.T) {
	h := newHarness(t)
	client, resp := loginClient(t, h.srv.URL, "admin", "pw123456")
	readBody(t, resp)

	port := freePort(t)
	r, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"server_name":     {"接收端"},
		"receive_enabled": {"1"},
		"receive_port":    {fmt.Sprint(port)},
	})
	if err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}
	readBody(t, r)

	key := h.cfg.Snapshot().ReceiveAPIKey
	remoteUUID := "cccccccc-3333-4333-8333-cccccccccccc"
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/api/report", port)

	post := func(body map[string]any, apiKey string) (*http.Response, error) {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}
		return (&http.Client{Timeout: 5 * time.Second}).Do(req)
	}

	// 等待接收端口就绪
	if !waitPort(t, fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second) {
		t.Fatalf("接收端口 %d 未在预期时间内监听", port)
	}

	payload := map[string]any{
		"uuid": remoteUUID,
		"name": "远端节点",
		"cpu":  12.5,
		"mem":  33.3,
		"ts":   time.Now().UnixMilli(),
	}

	// 错误 Key
	res, err := post(payload, "wrong-key")
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 Key 应返回 401，实际 %d", res.StatusCode)
	}

	// 正确 Key
	res, err = post(payload, key)
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("正确 Key 应返回 200，实际 %d", res.StatusCode)
	}

	// 非法 UUID
	bad := map[string]any{"uuid": "not-a-uuid", "name": "x", "cpu": 1, "mem": 1}
	res, err = post(bad, key)
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 UUID 应返回 400，实际 %d", res.StatusCode)
	}

	// 超出取值范围
	bad = map[string]any{"uuid": remoteUUID, "name": "x", "cpu": 120, "mem": 1}
	res, err = post(bad, key)
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("cpu 超范围应返回 400，实际 %d", res.StatusCode)
	}

	// GET 不被接受
	getRes, err := http.Get(endpoint)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	getRes.Body.Close()
	if getRes.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", getRes.StatusCode)
	}

	// 面板应能看到上报来的服务器（来源标记为 remote）
	ov, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	var overview struct {
		OK      bool `json:"ok"`
		Servers []struct {
			UUID   string  `json:"uuid"`
			Name   string  `json:"name"`
			CPU    float64 `json:"cpu"`
			Mem    float64 `json:"mem"`
			Source string  `json:"source"`
			IsSelf bool    `json:"is_self"`
			Count  int64   `json:"count"`
		} `json:"servers"`
	}
	if err := json.NewDecoder(ov.Body).Decode(&overview); err != nil {
		t.Fatalf("解析总览失败: %v", err)
	}
	ov.Body.Close()

	var found bool
	for _, s := range overview.Servers {
		if s.UUID == remoteUUID {
			found = true
			if s.Name != "远端节点" || s.CPU != 12.5 || s.Mem != 33.3 {
				t.Fatalf("上报数据不正确: %+v", s)
			}
			if s.Source != store.SourceRemote || s.IsSelf {
				t.Fatalf("上报数据来源标记不正确: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("面板中未找到上报来的服务器")
	}

	// 历史接口应能查到该服务器的数据
	hist, err := client.Get(h.srv.URL + "/api/history?uuid=" + remoteUUID)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	var hd struct {
		OK      bool           `json:"ok"`
		Name    string         `json:"name"`
		Records []store.Record `json:"records"`
		Stats   store.Stats    `json:"stats"`
	}
	if err := json.NewDecoder(hist.Body).Decode(&hd); err != nil {
		t.Fatalf("解析历史失败: %v", err)
	}
	hist.Body.Close()
	if !hd.OK || hd.Name != "远端节点" || len(hd.Records) != 1 || hd.Stats.Count != 1 {
		t.Fatalf("历史数据不正确: %+v", hd)
	}
	if hd.Records[0].Source != store.SourceRemote {
		t.Fatalf("历史记录来源应为上报: %+v", hd.Records[0])
	}
}

func TestDetailPage(t *testing.T) {
	h := newHarness(t)
	client, resp := loginClient(t, h.srv.URL, "admin", "pw123456")
	readBody(t, resp)

	// 不存在的服务器应跳回面板
	noRedirect := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// 复用登录 cookie
	for _, c := range client.Jar.Cookies(mustURL(t, h.srv.URL)) {
		noRedirect.Jar = mustJar(t, h.srv.URL, c)
	}
	r, err := noRedirect.Get(h.srv.URL + "/server/00000000-0000-4000-8000-000000000000")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusFound {
		t.Fatalf("未知服务器应跳回面板，实际 %d", r.StatusCode)
	}

	// 写入一条本机数据后访问详情页
	self := h.cfg.Snapshot()
	if err := h.st.Insert(store.Record{
		ServerUUID: self.UUID, ServerName: self.ServerName,
		CPU: 21.5, Mem: 66.6, Source: store.SourceLocal, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	page, err := client.Get(h.srv.URL + "/server/" + self.UUID)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	html := readBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("详情页应返回 200，实际 %d", page.StatusCode)
	}
	for _, want := range []string{`id="chart"`, "最近 1 个月", "历史记录", `id="detail-meta"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("详情页缺少元素 %q", want)
		}
	}
}

// ---------- 测试辅助 ----------

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// waitPort 轮询等待 addr（host:port）可连接。
func waitPort(t *testing.T, addr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 URL 失败: %v", err)
	}
	return u
}

func mustJar(t *testing.T, raw string, cookies ...*http.Cookie) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 jar 失败: %v", err)
	}
	jar.SetCookies(mustURL(t, raw), cookies)
	return jar
}

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
	"sync"
	"testing"
	"time"

	"server-monitor/internal/config"
	"server-monitor/internal/engine"
	"server-monitor/internal/store"
)

type harness struct {
	cfg *config.Manager
	st  *store.Store
	ws  *Server
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

	return &harness{cfg: cfg, st: st, ws: ws, srv: srv}
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

// csrfToken 从设置页中取出当前会话的 CSRF token。
func csrfToken(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	resp, err := client.Get(baseURL + "/settings")
	if err != nil {
		t.Fatalf("打开设置页失败: %v", err)
	}
	html := readBody(t, resp)

	const marker = `name="csrf" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatal("设置页未包含 CSRF token")
	}
	rest := html[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("CSRF token 格式异常")
	}
	if rest[:j] == "" {
		t.Fatal("CSRF token 为空")
	}
	return rest[:j]
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

	// 错误密码：应跳回带固定错误码的登录页
	_, resp = loginClient(t, h.srv.URL, "admin", "wrong-password")
	body := readBody(t, resp)
	if !strings.Contains(resp.Request.URL.RawQuery, "e=auth") {
		t.Fatalf("错误密码应跳回带错误码的登录页，实际 %s", resp.Request.URL.String())
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

	// 退出登录后失效（必须是带 CSRF 的 POST，GET 不再改变状态）
	lr, err := client.PostForm(h.srv.URL+"/logout", url.Values{
		"csrf": {csrfToken(t, client, h.srv.URL)},
	})
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

	csrf := csrfToken(t, client, h.srv.URL)
	port := freePort(t)

	// 开启数据接收
	r, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"csrf":            {csrf},
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
		"csrf":               {csrf},
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
		"csrf":               {csrf},
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
	off, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"csrf":        {csrf},
		"server_name": {"测试节点"},
	})
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
		"csrf":            {csrfToken(t, client, h.srv.URL)},
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

// 登录限流端到端：确认 handler 真的把失败记进了限流器，被拦时返回 429，
// 且退避结束后能自动恢复（不做硬锁定）。
func TestLoginThrottleEndToEnd(t *testing.T) {
	h := newHarness(t)

	// 注入可控时钟，避免测试真的等待 15 分钟退避。用互斥锁保护，
	// 因为 handler 在自己的 goroutine 里读取该时钟。
	var mu sync.Mutex
	cur := time.Now()
	h.ws.lim.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(d)
	}

	// 用不跟随重定向的客户端，才能观察到 302 / 429 这类原始状态码。
	raw := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	post := func(pass string) *http.Response {
		t.Helper()
		resp, err := raw.PostForm(h.srv.URL+"/login", url.Values{
			"username": {"admin"}, "password": {pass},
		})
		if err != nil {
			t.Fatalf("登录请求失败: %v", err)
		}
		return resp
	}

	// 一次真实失败：验证 handler 已把失败计入限流器。
	resp := post("wrong-password")
	readBody(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("首次失败应返回 302，实际 %d", resp.StatusCode)
	}
	if n := failureCount(t, h.ws.lim, "127.0.0.1"); n != 1 {
		t.Fatalf("handler 应把失败计入限流器，实际计数 %d", n)
	}

	// 补足失败次数以触发退避（直接操作限流器，省去真实响应延时）。
	for i := 1; i < loginFreeAttempts+1; i++ {
		h.ws.lim.fail("127.0.0.1")
	}

	// 退避期内应返回 429，并带 Retry-After 与友好提示。
	resp = post("wrong-password")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("退避期内应返回 429，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 响应应带 Retry-After 头")
	}
	if !strings.Contains(body, "过于频繁") {
		t.Fatal("429 页面应给出友好提示")
	}

	// 退避期内即使密码正确也必须拒绝，否则限流形同虚设。
	resp = post("pw123456")
	readBody(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("退避期内即使密码正确也应拒绝，实际 %d", resp.StatusCode)
	}

	// 越过退避期后应自动恢复，且正确密码能正常进入面板。
	advance(loginMaxBackoff + time.Second)
	_, resp = loginClient(t, h.srv.URL, "admin", "pw123456")
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("退避结束后应能正常登录，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "服务器面板") {
		t.Fatal("退避结束后正确密码应进入面板")
	}
	if n := failureCount(t, h.ws.lim, "127.0.0.1"); n != 0 {
		t.Fatalf("登录成功后应清除失败计数，实际 %d", n)
	}
}

// failureCount 读取限流器中某 IP 的累计失败次数（测试专用）。
func failureCount(t *testing.T, l *loginLimiter, ip string) int {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.ips[ip]; st != nil {
		return st.failures
	}
	return 0
}

// 登录页只认固定错误码，不得回显 URL 中的任意文本，否则可被用于钓鱼。
func TestLoginErrorTextIsNotReflected(t *testing.T) {
	h := newHarness(t)

	payload := "系统已停服，请联系管理员QQ123456"
	resp, err := http.Get(h.srv.URL + "/login?error=" + url.QueryEscape(payload))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readBody(t, resp)
	if strings.Contains(body, payload) {
		t.Fatal("登录页回显了 URL 中的任意文本，可被用于钓鱼")
	}
	if strings.Contains(body, "alert-error") {
		t.Fatal("未知错误码不应显示任何提示框")
	}

	// 已知错误码显示固定文案。
	resp2, err := http.Get(h.srv.URL + "/login?e=auth")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body2 := readBody(t, resp2)
	if !strings.Contains(body2, "账号或密码错误") {
		t.Fatal("已知错误码应显示固定文案")
	}
}

// 设置保存必须携带合法 CSRF token，否则拒绝且不产生任何副作用。
func TestSettingsRequiresCSRF(t *testing.T) {
	h := newHarness(t)
	client, resp := loginClient(t, h.srv.URL, "admin", "pw123456")
	readBody(t, resp)
	before := h.cfg.Snapshot().ServerName

	// 缺少 token
	noTok, err := client.PostForm(h.srv.URL+"/settings", url.Values{"server_name": {"被篡改"}})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, noTok)
	if noTok.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 CSRF token 应返回 403，实际 %d", noTok.StatusCode)
	}
	if h.cfg.Snapshot().ServerName != before {
		t.Fatal("CSRF 校验失败时不应修改配置")
	}

	// 伪造 token
	badTok, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"csrf":        {"0000000000000000000000000000000000000000000000000000000000000000"},
		"server_name": {"被篡改"},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, badTok)
	if badTok.StatusCode != http.StatusForbidden {
		t.Fatalf("伪造 CSRF token 应返回 403，实际 %d", badTok.StatusCode)
	}
	if h.cfg.Snapshot().ServerName != before {
		t.Fatal("伪造 token 时不应修改配置")
	}

	// 合法 token 应放行
	ok, err := client.PostForm(h.srv.URL+"/settings", url.Values{
		"csrf":        {csrfToken(t, client, h.srv.URL)},
		"server_name": {"合法修改"},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, ok)
	if h.cfg.Snapshot().ServerName != "合法修改" {
		t.Fatal("携带合法 CSRF token 的保存应生效")
	}
}

// 退出必须是带 CSRF 的 POST：GET 不得改变登录状态（否则可被跨站强制登出）。
func TestLogoutRequiresPostAndCSRF(t *testing.T) {
	h := newHarness(t)
	client, resp := loginClient(t, h.srv.URL, "admin", "pw123456")
	readBody(t, resp)

	// GET /logout 不应登出
	gr, err := client.Get(h.srv.URL + "/logout")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, gr)
	still, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if still.StatusCode != http.StatusOK {
		t.Fatalf("GET /logout 不应登出，接口应仍返回 200，实际 %d", still.StatusCode)
	}
	readBody(t, still)

	// 无 CSRF 的 POST 不应登出
	bad, err := client.PostForm(h.srv.URL+"/logout", url.Values{})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, bad)
	if bad.StatusCode != http.StatusForbidden {
		t.Fatalf("无 CSRF 的退出应返回 403，实际 %d", bad.StatusCode)
	}
	still2, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if still2.StatusCode != http.StatusOK {
		t.Fatalf("CSRF 校验失败的退出不应登出，实际 %d", still2.StatusCode)
	}
	readBody(t, still2)

	// 带 CSRF 的 POST 应登出
	ok, err := client.PostForm(h.srv.URL+"/logout", url.Values{
		"csrf": {csrfToken(t, client, h.srv.URL)},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, ok)
	after, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, after)
	if !strings.HasSuffix(after.Request.URL.Path, "/login") {
		t.Fatalf("带 CSRF 的退出应生效，实际仍访问到 %s", after.Request.URL.Path)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(h.srv.URL + "/login")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Fatalf("%s 应为 %q，实际 %q", k, want, got)
		}
	}

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("应下发 Content-Security-Policy")
	}
	for _, part := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, part) {
			t.Fatalf("CSP 应包含 %q，实际 %q", part, csp)
		}
	}
	// 关键断言：脚本不允许内联，否则 CSP 对 XSS 的防护大打折扣。
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") ||
		strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("script-src 不应允许 'unsafe-inline'，实际 %q", csp)
	}

	if resp.Header.Get("Permissions-Policy") == "" {
		t.Fatal("应下发 Permissions-Policy")
	}

	// 明文 HTTP 下不应下发 HSTS（会被浏览器忽略，且会掩盖尚未启用 HTTPS 的事实）。
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("明文 HTTP 下不应下发 HSTS，实际 %q", got)
	}
}

// 修改口令后，此前签发的所有登录态必须立即失效。
func TestSessionInvalidatedOnPasswordChange(t *testing.T) {
	h := newHarness(t)
	client, resp := loginClient(t, h.srv.URL, "admin", "pw123456")
	readBody(t, resp)

	ok, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("登录后接口应返回 200，实际 %d", ok.StatusCode)
	}
	readBody(t, ok)

	// 等价于部署脚本重新 init，或手工编辑配置后重启。
	if _, err := h.cfg.Update(func(c *config.Config) {
		c.WebPass = "a-brand-new-password"
	}); err != nil {
		t.Fatalf("修改密码失败: %v", err)
	}

	after, err := client.Get(h.srv.URL + "/api/overview")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	readBody(t, after)
	if !strings.HasSuffix(after.Request.URL.Path, "/login") {
		t.Fatalf("改密码后旧登录态应立即失效，实际仍访问到 %s", after.Request.URL.Path)
	}

	// 新口令应能正常登录。
	_, resp2 := loginClient(t, h.srv.URL, "admin", "a-brand-new-password")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("新口令应能登录，实际 %d", resp2.StatusCode)
	}
	readBody(t, resp2)
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

// Package web 提供 Web 登录、面板、服务器详情与设置页。
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"server-monitor/internal/config"
	"server-monitor/internal/engine"
	"server-monitor/internal/security"
	"server-monitor/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static/*
var staticFS embed.FS

const sessionCookie = "smd_session"

// Server 承载 Web 服务。
type Server struct {
	cfg  *config.Manager
	st   *store.Store
	eng  *engine.Engine
	tmpl *template.Template
	lim  *loginLimiter
}

// New 构建 Web 服务。
func New(cfg *config.Manager, st *store.Store, eng *engine.Engine) (*Server, error) {
	t, err := template.New("").Funcs(template.FuncMap{
		"pct": func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
		"ts": func(ms int64) string {
			if ms == 0 {
				return "-"
			}
			return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
		},
		"level": Level,
		"size":  HumanSize,
	}).ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("解析页面模板失败: %w", err)
	}
	return &Server{cfg: cfg, st: st, eng: eng, tmpl: t, lim: newLoginLimiter()}, nil
}

// Handler 返回 Web 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	staticRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticRoot)))

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginPost)
	// 退出必须是带 CSRF 校验的 POST：若允许 GET 退出，跨站的顶层跳转
	// 就能把用户强制登出。GET 保留一个无害跳转，兼容旧书签。
	mux.HandleFunc("POST /logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	})

	mux.HandleFunc("GET /{$}", s.auth(s.handlePanel))
	mux.HandleFunc("GET /server/{uuid}", s.auth(s.handleDetail))
	mux.HandleFunc("GET /settings", s.auth(s.handleSettings))
	mux.HandleFunc("POST /settings", s.auth(s.handleSettingsSave))
	mux.HandleFunc("GET /api/overview", s.auth(s.handleOverview))
	mux.HandleFunc("GET /api/history", s.auth(s.handleHistory))

	return securityHeaders(mux)
}

// Run 启动 Web 服务并在 ctx 结束时优雅关闭。
func (s *Server) Run(ctx context.Context) error {
	c := s.cfg.Snapshot()
	addr := fmt.Sprintf(":%d", c.WebPort)

	// 先显式监听再交给 http.Server：这样端口被占用时能得到可读的错误原因，
	// 而不是只在 systemd 日志里留下一句 "listen tcp ... bind"。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		hint := "可能已被其它程序占用"
		if c.WebPort < 1024 {
			hint += "，或该端口小于 1024 而服务不是以 root 运行"
		}
		return fmt.Errorf("Web 端口 %d 无法监听（%s）：%v", c.WebPort, hint, err)
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return srv.Serve(ln)
}

// ---------- 认证 ----------

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func (s *Server) currentUser(r *http.Request) (string, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return "", false
	}
	return s.verifyToken(ck.Value)
}

// sessionFingerprint 返回绑定当前凭证的短指纹。
//
// 把它放进令牌载荷后，登录态就与「当时的账号 + 口令 + 会话密钥」绑定：
// 任一变更都会让此前签发的所有 Cookie 立即失效，无需服务端会话表。
// 这正是「改了密码，旧登录态却还能用满 7 天」问题的解法。
func (s *Server) sessionFingerprint() string {
	c := s.cfg.Snapshot()
	sum := sha256.Sum256([]byte(c.SessionSecret + "|" + c.WebUser + "|" + c.WebPass))
	return hex.EncodeToString(sum[:8])
}

func (s *Server) sign(user string, exp int64) string {
	c := s.cfg.Snapshot()
	// 顺序为 指纹|过期时间|账号：把账号放最后，账号里即使含 "|" 也不会被截断。
	payload := strings.Join([]string{s.sessionFingerprint(), strconv.FormatInt(exp, 10), user}, "|")
	mac := hmac.New(sha256.New, []byte(c.SessionSecret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyToken(token string) (string, bool) {
	raw, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", false
	}
	c := s.cfg.Snapshot()
	mac := hmac.New(sha256.New, []byte(c.SessionSecret))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if !security.Equal(want, sig) {
		return "", false
	}

	parts := strings.SplitN(string(payload), "|", 3)
	if len(parts) != 3 {
		return "", false
	}
	fp, expStr, user := parts[0], parts[1], parts[2]

	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	// 凭证指纹不符说明口令 / 账号 / 会话密钥已变更，旧登录态一律失效。
	if !security.Equal(fp, s.sessionFingerprint()) {
		return "", false
	}
	if user != c.WebUser {
		return "", false
	}
	return user, true
}

// loginErrorMessages 把登录页的错误码映射为固定文案。
//
// 只认已知错误码、绝不回显 URL 里的任意文本：否则攻击者可构造一个
// 「域名正确、页面是真实登录页、但提示内容由攻击者自定义」的链接用于钓鱼
// （例如 /login?error=系统已停服，请联系管理员QQ…）。
var loginErrorMessages = map[string]string{
	"auth": "账号或密码错误",
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, "login.html", map[string]any{
		"Title":    "登录",
		"Error":    loginErrorMessages[r.URL.Query().Get("e")],
		"Hostname": s.cfg.Snapshot().ServerName,
	})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	ip := security.ClientIP(r)

	// 先做限流判定。被拦下的请求不计入失败数，退避会自然衰减。
	if d := s.lim.allow(ip); !d.Allowed {
		wait := d.RetryAfter.Round(time.Second)
		if wait < time.Second {
			wait = time.Second
		}
		// 限流日志按全局 30 秒一次输出，避免攻击者用海量请求把日志写成洪水。
		security.EventEvery("login_throttled", 30*time.Second, "login_throttled",
			"ip", ip, "scope", d.Scope, "retry_after", wait.String())
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		s.renderStatus(w, http.StatusTooManyRequests, "login.html", map[string]any{
			"Title":    "登录",
			"Error":    fmt.Sprintf("登录尝试过于频繁，请 %d 秒后重试", int(wait.Seconds())),
			"Hostname": s.cfg.Snapshot().ServerName,
		})
		return
	}

	c := s.cfg.Snapshot()

	// 两个比较都执行完再合并结果：&& 短路会泄露「用户名是否正确」的时序信息。
	// security.Equal 内部先做 SHA-256，比较长度恒定，长度差异也不再泄露。
	userOK := security.Equal(user, c.WebUser)
	passOK := security.Equal(pass, c.WebPass)

	if userOK && passOK && c.WebPass != "" {
		s.lim.success(ip)
		security.Event("login_success", "ip", ip, "user", user)

		exp := time.Now().Add(7 * 24 * time.Hour).Unix()
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    s.sign(user, exp),
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   7 * 24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	failures, banFor := s.lim.fail(ip)
	security.Event("login_failure", "ip", ip, "user", user, "failures", failures)
	if banFor > 0 {
		security.Event("login_backoff", "ip", ip, "failures", failures, "backoff", banFor.String())
	}

	// 轻微延时，降低暴力破解效率。
	time.Sleep(loginFailureDelay)
	http.Redirect(w, r, "/login?e=auth", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if !s.checkCSRF(r) {
		http.Error(w, "退出请求校验失败，请刷新页面后重试", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---------- CSRF ----------

// csrfToken 依据当前会话令牌派生 CSRF token。
//
// 采用「无状态派生」而非服务端存储：token = HMAC(会话密钥, "csrf|会话令牌")。
// 会话令牌是 HttpOnly 且同源的，跨站脚本读不到，因此攻击者无法为受害者的
// 会话算出合法 token。口令或会话密钥变更时 token 自动失效。
func (s *Server) csrfToken(r *http.Request) string {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return ""
	}
	c := s.cfg.Snapshot()
	mac := hmac.New(sha256.New, []byte(c.SessionSecret))
	mac.Write([]byte("csrf|" + ck.Value))
	return hex.EncodeToString(mac.Sum(nil))
}

// checkCSRF 校验表单提交的 CSRF token。
func (s *Server) checkCSRF(r *http.Request) bool {
	want := s.csrfToken(r)
	if want == "" {
		return false
	}
	return security.Equal(strings.TrimSpace(r.FormValue("csrf")), want)
}

// ---------- 页面 ----------

type baseData struct {
	Title  string
	Cfg    config.Config
	Active string
	CSRF   string
}

func (s *Server) base(r *http.Request, title, active string) baseData {
	return baseData{Title: title, Cfg: s.cfg.Snapshot(), Active: active, CSRF: s.csrfToken(r)}
}

func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Snapshot()
	latest, err := s.st.Latest()
	if err != nil {
		http.Error(w, "查询失败", http.StatusInternalServerError)
		return
	}
	type view struct {
		store.Latest
		IsSelf bool
	}
	servers := make([]view, 0, len(latest))
	for _, l := range latest {
		servers = append(servers, view{Latest: l, IsSelf: l.ServerUUID == c.UUID})
	}
	s.render(w, "panel.html", struct {
		baseData
		Servers []view
	}{s.base(r, "面板", "panel"), servers})
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	c := s.cfg.Snapshot()

	name, err := s.st.Name(uuid)
	if err != nil {
		http.Error(w, "查询失败", http.StatusInternalServerError)
		return
	}
	if name == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, "detail.html", struct {
		baseData
		UUID   string
		Name   string
		IsSelf bool
	}{s.base(r, name, "panel"), uuid, name, uuid == c.UUID})
}

type settingsData struct {
	baseData
	Saved     bool
	ErrorMsg  string
	DBSize    int64
	HasDBSz   bool
	Listening int
	IPs       []string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, "", r.URL.Query().Get("saved") == "1")
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, code int, errMsg string, saved bool) {
	c := s.cfg.Snapshot()
	data := settingsData{
		baseData:  s.base(r, "设置", "settings"),
		Saved:     saved,
		ErrorMsg:  errMsg,
		Listening: s.eng.Running(),
		IPs:       LocalIPs(),
	}
	if c.ReceiveEnabled {
		if n, err := s.st.Size(); err == nil {
			data.DBSize, data.HasDBSz = n, true
		}
	}
	s.renderStatus(w, code, "settings.html", data)
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, "表单解析失败", false)
		return
	}
	// CSRF 校验必须在任何写操作之前。SameSite=Lax 已能挡住跨站 POST，
	// 这里是第二道防线，也覆盖了该属性不被支持或未来被放宽的情况。
	if !s.checkCSRF(r) {
		s.renderSettings(w, r, http.StatusForbidden,
			"表单校验失败（页面可能已过期），请刷新页面后重试", false)
		return
	}
	_, err := s.cfg.Update(func(c *config.Config) {
		c.ServerName = strings.TrimSpace(r.FormValue("server_name"))

		c.ReceiveEnabled = r.FormValue("receive_enabled") != ""
		// 关闭功能时对应输入框不会随表单提交，此处仅在字段存在时更新，保留原值。
		if _, ok := r.Form["receive_port"]; ok {
			if p, e := strconv.Atoi(strings.TrimSpace(r.FormValue("receive_port"))); e == nil {
				c.ReceivePort = p
			}
		}

		c.ReportEnabled = r.FormValue("report_enabled") != ""
		if _, ok := r.Form["report_target_ip"]; ok {
			c.ReportTargetIP = strings.TrimSpace(r.FormValue("report_target_ip"))
		}
		if _, ok := r.Form["report_target_port"]; ok {
			if p, e := strconv.Atoi(strings.TrimSpace(r.FormValue("report_target_port"))); e == nil {
				c.ReportTargetPort = p
			}
		}
		if _, ok := r.Form["report_interval"]; ok {
			if iv, e := strconv.Atoi(strings.TrimSpace(r.FormValue("report_interval"))); e == nil {
				c.ReportInterval = iv
			}
		}
		if _, ok := r.Form["report_api_key"]; ok {
			c.ReportAPIKey = strings.TrimSpace(r.FormValue("report_api_key"))
		}
	})
	if err != nil {
		s.renderSettings(w, r, http.StatusOK, err.Error(), false)
		return
	}
	// 配置变更会触发 engine 的热重启回调（见 main.go 中的 OnChange 注册）。
	http.Redirect(w, r, "/settings?saved=1", http.StatusFound)
}

// ---------- 接口 ----------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	latest, err := s.st.Latest()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "查询失败"})
		return
	}
	c := s.cfg.Snapshot()
	type item struct {
		store.Latest
		IsSelf bool `json:"is_self"`
	}
	items := make([]item, 0, len(latest))
	for _, l := range latest {
		items = append(items, item{Latest: l, IsSelf: l.ServerUUID == c.UUID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"self":     c.UUID,
		"selfName": c.ServerName,
		"now":      time.Now().UnixMilli(),
		"servers":  items,
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uuid := q.Get("uuid")
	if !config.IsUUID(uuid) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "服务器识别码不合法"})
		return
	}
	now := time.Now()
	to := parseMs(q.Get("to"), now.UnixMilli())
	from := parseMs(q.Get("from"), now.AddDate(0, -1, 0).UnixMilli())
	if from > to {
		from, to = to, from
	}

	points, err := s.st.Series(uuid, from, to, 720)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "查询失败"})
		return
	}
	records, err := s.st.History(uuid, from, to, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "查询失败"})
		return
	}
	stat, err := s.st.Stat(uuid, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "查询失败"})
		return
	}
	name, _ := s.st.Name(uuid)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uuid":    uuid,
		"name":    name,
		"from":    from,
		"to":      to,
		"is_self": uuid == s.cfg.Snapshot().UUID,
		"points":  points,
		"records": records,
		"stats":   stat,
	})
}

// ---------- 工具 ----------

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	s.renderStatus(w, http.StatusOK, name, data)
}

// renderStatus 与 render 相同，但可指定 HTTP 状态码（例如限流时的 429）。
func (s *Server) renderStatus(w http.ResponseWriter, code int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// contentSecurityPolicy 是页面的 CSP。
//
// script-src 能收紧到 'self'，是因为模板与 app.js 中没有任何内联脚本、
// 也没有内联事件处理器（onclick 之类），JS 全部来自 /static/app.js。
// style-src 必须保留 'unsafe-inline'：进度条宽度（panel.html、app.js）
// 与图表提示配色使用了内联 style 属性，去掉会直接破坏渲染。
// 样式注入的危害远小于脚本注入，这一取舍是有意的。
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"object-src 'none'"

// permissionsPolicy 关闭页面用不到的所有浏览器特性，缩小被滥用面。
const permissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), " +
	"usb=(), serial=(), midi=(), magnetometer=(), gyroscope=()"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Permissions-Policy", permissionsPolicy)
		// HSTS 只在请求确实走 TLS 时下发：明文请求上的该头部会被浏览器忽略，
		// 无条件下发反而会掩盖「其实还没启用 HTTPS」这一事实。
		// 真正的 HTTPS 支持属于后续工作，届时这里会自动开始生效。
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func parseMs(v string, def int64) int64 {
	if strings.TrimSpace(v) == "" {
		return def
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t.UnixMilli()
		}
	}
	return def
}

// LocalIPs 返回本机非回环 IPv4 地址，用于在设置页提示上报时应填写的 IP。
func LocalIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

// ---------- 视图辅助 ----------

// Level 依据占用率返回展示等级，用于进度条配色。
func Level(v float64) string {
	switch {
	case v >= 85:
		return "danger"
	case v >= 60:
		return "warn"
	default:
		return "ok"
	}
}

// HumanSize 把字节数格式化为易读字符串。
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	digits := 1
	if v >= 100 {
		digits = 0
	}
	return strconv.FormatFloat(v, 'f', digits, 64) + " " + units[i]
}

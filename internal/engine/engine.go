// Package engine 是程序运行时核心：按配置驱动「采集 + 上报」循环与「数据接收」服务。
//
// 采集频率与上报频率绑定，统一由上报设置中的频率决定；
// 上报关闭时按 5 秒采集。开关或端口变更后调用 Apply 即可热重启对应循环。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"server-monitor/internal/collect"
	"server-monitor/internal/config"
	"server-monitor/internal/security"
	"server-monitor/internal/store"
)

// reportPayload 是上报接口的请求体。同时兼容下划线命名，便于其它实现接入。
type reportPayload struct {
	UUID      string   `json:"uuid"`
	ServerID  string   `json:"server_uuid"`
	Name      string   `json:"name"`
	ServerNme string   `json:"server_name"`
	CPU       *float64 `json:"cpu"`
	CPUUsage  *float64 `json:"cpu_usage"`
	Mem       *float64 `json:"mem"`
	MemUsage  *float64 `json:"mem_usage"`
	TS        int64    `json:"ts"`
	CreatedAt int64    `json:"created_at"`
	// Sys 是发行版 / 内核 / 开机时刻，只在信息变化时随上报发送一次，
	// 因此是可选字段：缺失时接收端保留此前记录的值。
	Sys *sysPayload `json:"sys,omitempty"`
}

// sysPayload 是上报体中的系统信息块。
type sysPayload struct {
	OS     string `json:"os"`
	Kernel string `json:"kernel"`
	BootAt int64  `json:"boot_at"` // 开机时刻（Unix 毫秒）
	Uptime int64  `json:"uptime"`  // 运行时长（秒），供不解析 boot_at 的客户端参考
}

// 系统信息的最长接受长度（按字符计），防止畸形上报把库撑大。
const (
	maxOSNameLen = 64
	maxKernelLen = 64
)

// sysInfoResend 是系统信息的重发间隔。
//
// 正常情况下它只在变化时发送一次；这里再定时重发是为了兜底：
// 例如接收端换过数据库、清过库，或某次上报恰好失败。
const sysInfoResend = 30 * time.Minute

// Engine 负责运行时的采集、上报与接收。
type Engine struct {
	parent context.Context
	cfg    *config.Manager
	st     *store.Store

	mu            sync.Mutex
	collectCancel context.CancelFunc
	recvCancel    context.CancelFunc
	recvPort      int

	client *http.Client
}

// New 创建运行时引擎。
//
// 引擎会自动订阅配置变更：设置保存后无需调用方干预，采集 / 上报 / 接收循环会自行按新配置重启。
func New(parent context.Context, cfg *config.Manager, st *store.Store) *Engine {
	if parent == nil {
		parent = context.Background()
	}
	e := &Engine{
		parent: parent,
		cfg:    cfg,
		st:     st,
		client: &http.Client{Timeout: 5 * time.Second},
	}
	cfg.OnChange(func(config.Config) { e.Apply() })
	return e
}

// Apply 依据当前配置（重新）启动采集 / 上报 / 接收。
// 配置每次保存后都会被调用，因此这里必须幂等。
func (e *Engine) Apply() {
	ctx := e.parent
	c := e.cfg.Snapshot()

	e.mu.Lock()
	defer e.mu.Unlock()

	// 采集 + 上报：间隔可能变化，直接整体重启。
	if e.collectCancel != nil {
		e.collectCancel()
	}
	cctx, cancel := context.WithCancel(ctx)
	e.collectCancel = cancel
	go e.collectLoop(cctx, c.EffectiveInterval())

	// 接收 / 查询 API 共用端口：任一开启就监听；端口变化时重启监听。
	switch {
	case !c.DataPortOpen():
		if e.recvCancel != nil {
			e.recvCancel()
			e.recvCancel = nil
			e.recvPort = 0
		}
	case e.recvCancel == nil || e.recvPort != c.ReceivePort:
		if e.recvCancel != nil {
			e.recvCancel()
		}
		rctx, rcancel := context.WithCancel(ctx)
		e.recvCancel = rcancel
		e.recvPort = c.ReceivePort
		go e.receiveLoop(rctx, c.ReceivePort)
	}
}

// Running 返回当前接收服务的监听端口（0 表示未开启）。
func (e *Engine) Running() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recvPort
}

func (e *Engine) collectLoop(ctx context.Context, interval time.Duration) {
	sampler := collect.New()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 系统信息的上报状态随采集循环重建：重启循环（改配置）后补发一次，
	// 代价只是一个几十字节的字段，换来的是接收端不会长期缺信息。
	state := &sysSendState{}

	for {
		e.sampleOnce(sampler, state)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sysSendState 记录上一次把系统信息写库 / 上报的时间与内容。
type sysSendState struct {
	key string
	at  time.Time
}

// due 判断本次采样是否需要携带系统信息：内容变了（升级、重启）或距上次
// 发送超过了 sysInfoResend。绝大多数采样间隔里它都返回 false，
// 因此发行版 / 内核 / 运行时长不会每 2 秒重复发一遍。
func (s *sysSendState) due(info collect.SysInfo, now time.Time) bool {
	if s.key != info.Key() {
		return true
	}
	return now.Sub(s.at) >= sysInfoResend
}

func (e *Engine) sampleOnce(sampler interface {
	Read() (collect.Sample, error)
}, state *sysSendState) {
	smp, err := sampler.Read()
	if err != nil {
		return
	}
	c := e.cfg.Snapshot()
	now := time.Now()

	withSys := state.due(smp.Sys, now)
	if withSys {
		// 先落本地库：面板与查询 API 都读这里，本机也不能例外。
		if err := e.st.UpsertInfo(store.Info{
			ServerUUID: c.UUID,
			OS:         smp.Sys.OS,
			Kernel:     smp.Sys.Kernel,
			BootAt:     smp.Sys.BootAt,
			UpdatedAt:  now.UnixMilli(),
		}); err == nil {
			state.key, state.at = smp.Sys.Key(), now
		}
	}

	rec := store.Record{
		ServerUUID: c.UUID,
		ServerName: c.ServerName,
		CPU:        smp.CPU,
		Mem:        smp.Mem,
		Source:     store.SourceLocal,
		CreatedAt:  now.UnixMilli(),
	}
	if err := e.st.Insert(rec); err != nil {
		return
	}
	if c.ReportEnabled {
		e.report(c, rec, smp.Sys, withSys)
	}
}

func (e *Engine) report(c config.Config, rec store.Record, sys collect.SysInfo, withSys bool) {
	p := reportPayload{
		UUID: rec.ServerUUID,
		Name: rec.ServerName,
		CPU:  &rec.CPU,
		Mem:  &rec.Mem,
		TS:   rec.CreatedAt,
	}
	if withSys {
		p.Sys = &sysPayload{
			OS:     sys.OS,
			Kernel: sys.Kernel,
			BootAt: sys.BootAt,
			Uptime: int64(sys.UptimeAt(time.Now()) / time.Second),
		}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	url := fmt.Sprintf("http://%s:%d/api/report", c.ReportTargetIP, c.ReportTargetPort)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.ReportAPIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

func (e *Engine) receiveLoop(ctx context.Context, port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", e.handleReport)
	// 数据接收端口上同样提供查询（仅接收功能开启时该端口才会监听）；
	// Web 端口上另有一份，见 web.Handler。
	mux.HandleFunc("/api/servers", e.ServeServersAPI)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
		// 千台规模的接收端参数：只读 64KB 请求体、短头部超时，
		// 空闲连接保留一段时间以便上报端复用（避免每 2 秒重建 TCP/TLS 之外的握手）。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		// Shutdown 会等待在途请求处理完（写入队列里的数据不会丢）。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "数据接收端口 %d 监听失败：%v\n", port, err)
	}
}

func (e *Engine) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, false, "仅支持 POST")
		return
	}

	c := e.cfg.Snapshot()
	if !c.ReceiveEnabled || c.ReceiveAPIKey == "" {
		writeJSON(w, http.StatusForbidden, false, "接收功能未开启")
		return
	}
	key := apiKeyFrom(r)
	// 常量时间比较：普通 != 会在首个不同字节处短路，理论上可被用来逐字节
	// 猜测 Key。该接口未认证即可访问，因此鉴权失败日志按 IP 限频，防止
	// 攻击者用海量请求把日志写成洪水。
	if !security.Equal(key, c.ReceiveAPIKey) {
		security.EventEvery("report_auth:"+security.ClientIP(r), time.Minute,
			"report_auth_failure", "ip", security.ClientIP(r))
		writeJSON(w, http.StatusUnauthorized, false, "API-Key 校验失败")
		return
	}

	// 上报体很小，限制上限可防止畸形请求占用内存。
	var p reportPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, false, "请求体解析失败")
		return
	}

	uuid := firstNonEmpty(p.UUID, p.ServerID)
	if !config.IsUUID(uuid) {
		writeJSON(w, http.StatusBadRequest, false, "服务器识别码不合法")
		return
	}
	name := firstNonEmpty(p.Name, p.ServerNme)
	if name == "" {
		name = uuid
	}
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}

	var cpu, mem float64
	if p.CPU != nil {
		cpu = *p.CPU
	} else if p.CPUUsage != nil {
		cpu = *p.CPUUsage
	}
	if p.Mem != nil {
		mem = *p.Mem
	} else if p.MemUsage != nil {
		mem = *p.MemUsage
	}
	if cpu < 0 || cpu > 100 || mem < 0 || mem > 100 {
		writeJSON(w, http.StatusBadRequest, false, "cpu / mem 取值需在 0~100 之间")
		return
	}

	now := time.Now().UnixMilli()
	ts := p.TS
	if ts == 0 {
		ts = p.CreatedAt
	}
	// 上报端时钟明显异常时以接收端时间为准，避免污染时间轴。
	if ts <= 0 || ts > now+60_000 || ts < now-3600_000 {
		ts = now
	}

	rec := store.Record{
		ServerUUID: uuid,
		ServerName: name,
		CPU:        round1(cpu),
		Mem:        round1(mem),
		Source:     store.SourceRemote,
		CreatedAt:  ts,
	}
	if err := e.st.Insert(rec); err != nil {
		writeJSON(w, http.StatusInternalServerError, false, "写入失败")
		return
	}

	// 系统信息块是可选的：只有上报端发现信息变化时才会带，
	// 因此这里不能把它当成必填项，也不能因为缺失就清空已有记录。
	if p.Sys != nil {
		if err := e.st.UpsertInfo(store.Info{
			ServerUUID: uuid,
			OS:         truncateRunes(p.Sys.OS, maxOSNameLen),
			Kernel:     truncateRunes(p.Sys.Kernel, maxKernelLen),
			BootAt:     sanitizeBootAt(p.Sys.BootAt, now),
			UpdatedAt:  now,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, false, "写入失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, true, "")
}

// ServeServersAPI 对外提供「本机存储的全部服务器信息」的只读查询。
//
// 它同时挂在 Web 端口与数据接收端口上：Web 端口是默认入口（只需放行一个端口，
// 也便于用反向代理加 TLS）；数据接收端口那一路留给只放行该端口的采集网环境。
// 两个端口上的鉴权、返回体完全一致，用的是同一个 API-Key。
//
// 与 /api/report 共用鉴权形式，但使用独立的 API-Key：接收 Key 需要
// 分发给每一台上报服务器（它们只需写权限），若同时能读全量数据，
// 任何一台被攻陷的服务器都能拉走整个机群的状况。
func (e *Engine) ServeServersAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, false, "仅支持 GET")
		return
	}

	c := e.cfg.Snapshot()
	if !c.APIEnabled || c.APIKey == "" {
		writeJSON(w, http.StatusForbidden, false, "查询 API 未开启")
		return
	}
	if !security.Equal(apiKeyFrom(r), c.APIKey) {
		security.EventEvery("api_auth:"+security.ClientIP(r), time.Minute,
			"api_auth_failure", "ip", security.ClientIP(r))
		writeJSON(w, http.StatusUnauthorized, false, "API-Key 校验失败")
		return
	}

	latest, err := e.st.Latest()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, false, "查询失败")
		return
	}
	infos, err := e.st.Infos()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, false, "查询失败")
		return
	}

	now := time.Now()
	items := make([]apiServer, 0, len(latest))
	for _, l := range latest {
		inf := infos[l.ServerUUID]
		items = append(items, apiServer{
			ServerUUID: l.ServerUUID,
			Name:       l.ServerName,
			CPU:        l.CPU,
			Mem:        l.Mem,
			Source:     l.Source,
			TS:         l.CreatedAt,
			Count:      l.Count,
			IsSelf:     l.ServerUUID == c.UUID,
			OS:         inf.OS,
			Kernel:     inf.Kernel,
			BootAt:     inf.BootAt,
			Uptime:     int64(inf.UptimeAt(now) / time.Second),
		})
	}

	writeJSONBody(w, http.StatusOK, apiResponse{
		OK:       true,
		Now:      now.UnixMilli(),
		Self:     c.UUID,
		SelfName: c.ServerName,
		Count:    len(items),
		Servers:  items,
	})
}

// apiServer 是查询接口中单台服务器的信息。
type apiServer struct {
	ServerUUID string  `json:"uuid"`
	Name       string  `json:"name"`
	CPU        float64 `json:"cpu"`
	Mem        float64 `json:"mem"`
	Source     string  `json:"source"`
	TS         int64   `json:"ts"`
	Count      int64   `json:"count"`
	IsSelf     bool    `json:"is_self"`
	OS         string  `json:"os"`
	Kernel     string  `json:"kernel"`
	BootAt     int64   `json:"boot_at"`
	Uptime     int64   `json:"uptime"` // 秒；boot_at 未知时为 0
}

// apiResponse 是查询接口的完整响应体。错误响应走 {ok:false,error:...} 的简单形态。
type apiResponse struct {
	OK       bool        `json:"ok"`
	Now      int64       `json:"now"`
	Self     string      `json:"self"`
	SelfName string      `json:"self_name"`
	Count    int         `json:"count"`
	Servers  []apiServer `json:"servers"`
}

// apiKeyFrom 从请求头中取出 API-Key，兼容 X-API-Key 与 Bearer 两种形式。
//
// 刻意不支持 ?key= 查询参数：URL 会进入各级访问日志与浏览器历史，
// 把长期有效的凭据放在里面等于公开。
func apiKeyFrom(r *http.Request) string {
	// 不做 TrimSpace：Key 必须逐字节精确匹配。真实请求的头部值会被
	// 传输层去掉首尾空白，这里的严格只影响刻意构造的请求。
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	auth := r.Header.Get("Authorization")
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, ok bool, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	// 成功路径是热点（每秒数百次），直接写固定字节，省掉一次 map + Encoder 分配。
	if msg == "" {
		if ok {
			_, _ = w.Write([]byte(`{"ok":true}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":false}`))
		}
		return
	}
	resp := map[string]any{"ok": ok, "error": msg}
	_ = json.NewEncoder(w).Encode(resp)
}

// writeJSONBody 输出任意结构体（查询接口用，请求频率低，无需抠分配）。
func writeJSONBody(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 数据每几秒就变，避免中间层缓存导致第三方读到过期快照。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// truncateRunes 按字符数截断，避免把超长字段写进库。
func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max])
}

// sanitizeBootAt 丢弃明显不可能的开机时刻（未来的时间、负数），
// 避免一台时钟错乱的服务器把「运行时长」显示成几百天或负数。
func sanitizeBootAt(bootAt, now int64) int64 {
	if bootAt <= 0 || bootAt > now+60_000 {
		return 0
	}
	return bootAt
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

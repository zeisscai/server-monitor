// Package engine 是程序运行时核心：按配置驱动「采集 + 上报」循环与「数据接收」服务。
//
// 采集频率与上报频率绑定，统一由上报设置中的频率决定；
// 上报关闭时按 5 秒采集。开关或端口变更后调用 Apply 即可热重启对应循环。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"server-monitor/internal/collect"
	"server-monitor/internal/config"
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
}

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

	// 接收服务：仅在开启时监听；端口变化时重启监听。
	switch {
	case !c.ReceiveEnabled:
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

	for {
		e.sampleOnce(sampler)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) sampleOnce(sampler interface {
	Read() (collect.Sample, error)
}) {
	smp, err := sampler.Read()
	if err != nil {
		return
	}
	c := e.cfg.Snapshot()
	rec := store.Record{
		ServerUUID: c.UUID,
		ServerName: c.ServerName,
		CPU:        smp.CPU,
		Mem:        smp.Mem,
		Source:     store.SourceLocal,
		CreatedAt:  time.Now().UnixMilli(),
	}
	if err := e.st.Insert(rec); err != nil {
		return
	}
	if c.ReportEnabled {
		e.report(c, rec)
	}
}

func (e *Engine) report(c config.Config, rec store.Record) {
	body, err := json.Marshal(reportPayload{
		UUID: rec.ServerUUID,
		Name: rec.ServerName,
		CPU:  &rec.CPU,
		Mem:  &rec.Mem,
		TS:   rec.CreatedAt,
	})
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

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	_ = srv.ListenAndServe()
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
	key := r.Header.Get("X-API-Key")
	if key == "" {
		key = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	}
	if key != c.ReceiveAPIKey {
		writeJSON(w, http.StatusUnauthorized, false, "API-Key 校验失败")
		return
	}

	var p reportPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&p); err != nil {
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
	writeJSON(w, http.StatusOK, true, "")
}

func writeJSON(w http.ResponseWriter, code int, ok bool, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	resp := map[string]any{"ok": ok}
	if msg != "" {
		resp["error"] = msg
	}
	_ = json.NewEncoder(w).Encode(resp)
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

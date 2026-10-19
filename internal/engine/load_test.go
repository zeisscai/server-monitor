package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"server-monitor/internal/config"
	"server-monitor/internal/store"
)

// 负载测试：模拟千台服务器同时上报，验证接收端的吞吐与延迟。
//
// 默认跳过，需要显式开启：
//
//	SMD_LOAD=1 /usr/local/go/bin/go test ./internal/engine -run TestLoad -v -timeout 15m
//
// 可用环境变量：
//
//	SMD_NODES=1000   模拟的服务器数量
//	SMD_SEED=2000000 先灌入多少行历史数据（让写入面对真实的索引体积）
//	SMD_PANEL=1      额外模拟面板每 500ms 刷新一次（考察读写是否互相阻塞）
//
// 只启动接收服务（不经 Apply 拉起采集循环），避免本机采集记录干扰计数。
type receiver struct {
	st       *store.Store
	endpoint string
	key      string
	client   *http.Client
}

func startReceiver(t *testing.T) *receiver {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(dir + "/config.json")
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	port := freePort(t)
	if _, err := cfg.Update(func(c *config.Config) {
		c.ReceiveEnabled = true
		c.ReceivePort = port
		c.WebPort = 18080
	}); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	key := cfg.Snapshot().ReceiveAPIKey

	st, err := store.Open(dir + "/data")
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = st.Close()
	})
	e := New(ctx, cfg, st)
	go e.receiveLoop(ctx, port)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if !waitPort(addr, 5*time.Second) {
		t.Fatalf("接收端口 %d 未就绪", port)
	}

	// 独立 Transport：连接池要能容纳上千条并发长连接。
	tr := &http.Transport{
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 4096,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &receiver{
		st:       st,
		endpoint: "http://" + addr + "/api/report",
		key:      key,
		client:   &http.Client{Transport: tr, Timeout: 10 * time.Second},
	}
}

func (r *receiver) post(body []byte) error {
	req, err := http.NewRequest(http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", r.key)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	// 必须读完响应体再关闭，否则连接不会被复用（压测端会打爆本地端口）。
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("状态码 %d", resp.StatusCode)
	}
	return nil
}

// payload 生成第 i 台服务器的上报体。
func payload(i int) []byte {
	raw, _ := json.Marshal(map[string]any{
		"uuid": fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
		"name": fmt.Sprintf("node-%04d", i),
		"cpu":  float64(i%100) + 0.5,
		"mem":  40.5,
		"ts":   time.Now().UnixMilli(),
	})
	return raw
}

func seedRows(t *testing.T, st *store.Store, n int) {
	t.Helper()
	if n <= 0 {
		return
	}
	t0 := time.Now()
	buf := make([]store.Record, 0, 2000)
	base := time.Now().Add(-72 * time.Hour).UnixMilli()
	for i := 0; i < n/2000; i++ {
		uuid := fmt.Sprintf("11111111-0000-4000-8000-%012d", i)
		buf = buf[:0]
		for j := 0; j < 2000; j++ {
			buf = append(buf, store.Record{
				ServerUUID: uuid, ServerName: "seed",
				CPU: 10, Mem: 20, Source: store.SourceRemote,
				CreatedAt: base + int64(j)*1000,
			})
		}
		if err := st.InsertBatch(buf); err != nil {
			t.Fatalf("灌数失败: %v", err)
		}
	}
	t.Logf("[预热] 灌入历史 %d 行，耗时 %v", n, time.Since(t0).Round(time.Millisecond))
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func TestLoadReports(t *testing.T) {
	if os.Getenv("SMD_LOAD") != "1" {
		t.Skip("设置 SMD_LOAD=1 后运行负载测试")
	}
	nodes := envInt("SMD_NODES", 1000)
	r := startReceiver(t)
	seedRows(t, r.st, envInt("SMD_SEED", 0))
	baseCount, _ := r.st.Count()

	// 可选：模拟面板每 500ms 刷新一次总览。
	var panelStop int32
	var panelMax int64
	if os.Getenv("SMD_PANEL") == "1" {
		go func() {
			for atomic.LoadInt32(&panelStop) == 0 {
				t0 := time.Now()
				if _, err := r.st.Latest(); err != nil {
					t.Logf("[面板] Latest 失败: %v", err)
				}
				d := int64(time.Since(t0))
				for {
					cur := atomic.LoadInt64(&panelMax)
					if d <= cur || atomic.CompareAndSwapInt64(&panelMax, cur, d) {
						break
					}
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()
	}

	payloads := make([][]byte, nodes)
	for i := range payloads {
		payloads[i] = payload(i)
	}

	var (
		mu      sync.Mutex
		latency []time.Duration
		okN     int64
		failN   int64
		errSeen = map[string]int{}
	)
	record := func(d time.Duration, err error) {
		if err != nil {
			atomic.AddInt64(&failN, 1)
			mu.Lock()
			errSeen[err.Error()]++
			mu.Unlock()
			return
		}
		atomic.AddInt64(&okN, 1)
		mu.Lock()
		latency = append(latency, d)
		mu.Unlock()
	}

	// 1) 突发：全部节点同时上报（错开 300µs，模拟服务重启后客户端陆续重连，
	//    同时避开 macOS 的 accept 队列上限 kern.ipc.somaxconn=128）。
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(nodes)
	for i := 0; i < nodes; i++ {
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 300 * time.Microsecond)
			t0 := time.Now()
			err := r.post(payloads[i])
			record(time.Since(t0), err)
		}(i)
	}
	wg.Wait()
	burstElapsed := time.Since(start)
	burstOK := okN
	t.Logf("[突发] %d 台同时上报：耗时 %v，成功 %d，失败 %d", nodes, burstElapsed.Round(time.Millisecond), okN, failN)
	mu.Lock()
	for k, v := range errSeen {
		t.Logf("[突发错误] %s × %d", k, v)
	}
	errSeen = map[string]int{}
	mu.Unlock()

	// 2) 持续：全部节点每 2 秒上报一次，持续 8 秒
	atomic.StoreInt64(&okN, 0)
	atomic.StoreInt64(&failN, 0)
	mu.Lock()
	latency = nil
	mu.Unlock()

	deadline := time.Now().Add(8 * time.Second)
	start = time.Now()
	wg.Add(nodes)
	for i := 0; i < nodes; i++ {
		go func(i int) {
			defer wg.Done()
			// 首次上报在 1 秒内均匀错开（真实场景各节点本就不同步）。
			time.Sleep(time.Duration(i) * time.Second / time.Duration(nodes))
			for time.Now().Before(deadline) {
				t0 := time.Now()
				err := r.post(payloads[i])
				record(time.Since(t0), err)
				time.Sleep(2 * time.Second)
			}
		}(i)
	}
	wg.Wait()
	steadyElapsed := time.Since(start)

	mu.Lock()
	lat := append([]time.Duration(nil), latency...)
	for k, v := range errSeen {
		t.Logf("[持续错误] %s × %d", k, v)
	}
	mu.Unlock()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(float64(len(lat)-1)*p)]
	}
	total := okN + failN
	if total > 0 {
		t.Logf("[持续] %v 内 %d 次上报（%.0f req/s），成功 %d，失败 %d",
			steadyElapsed.Round(time.Millisecond), total, float64(total)/steadyElapsed.Seconds(), okN, failN)
		t.Logf("[延迟] p50=%v p95=%v p99=%v max=%v",
			pct(0.50).Round(time.Microsecond), pct(0.95).Round(time.Microsecond),
			pct(0.99).Round(time.Microsecond), pct(1).Round(time.Microsecond))
	}

	atomic.StoreInt32(&panelStop, 1)
	if os.Getenv("SMD_PANEL") == "1" {
		t.Logf("[面板] Latest 最慢一次 %v", time.Duration(atomic.LoadInt64(&panelMax)).Round(time.Millisecond))
	}

	// 3) 落库校验：等待写入队列排空
	want := okN + burstOK + baseCount
	var got int64
	var err error
	for i := 0; i < 60; i++ {
		if got, err = r.st.Count(); err == nil && got >= want {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("[落库] 期望 %d 条，实际 %d 条", want, got)
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if failN > 0 {
		t.Fatalf("存在 %d 次失败上报", failN)
	}
	if got < want {
		t.Fatalf("落库数量不足：期望 %d，实际 %d", want, got)
	}
}

// TestLoadThroughput 用固定并发压到极限，观察接收端的吞吐上限。
func TestLoadThroughput(t *testing.T) {
	if os.Getenv("SMD_LOAD") != "1" {
		t.Skip("设置 SMD_LOAD=1 后运行负载测试")
	}
	workers := envInt("SMD_WORKERS", 200)
	secs := envInt("SMD_SECS", 5)

	r := startReceiver(t)
	seedRows(t, r.st, envInt("SMD_SEED", 0))

	payloads := make([][]byte, workers)
	for i := range payloads {
		payloads[i] = payload(i)
	}

	var okN, failN int64
	var emu sync.Mutex
	errSeen := map[string]int{}
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if err := r.post(payloads[i]); err != nil {
					atomic.AddInt64(&failN, 1)
					emu.Lock()
					errSeen[err.Error()]++
					emu.Unlock()
					continue
				}
				atomic.AddInt64(&okN, 1)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("[吞吐] %d 并发 × %v：成功 %d 次，失败 %d 次，%.0f req/s",
		workers, elapsed.Round(time.Millisecond), okN, failN, float64(okN)/elapsed.Seconds())
	emu.Lock()
	for k, v := range errSeen {
		t.Logf("[错误] %s × %d", k, v)
	}
	emu.Unlock()

	var got int64
	for i := 0; i < 60; i++ {
		if got, _ = r.st.Count(); got >= okN {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("[落库] 成功 %d 条，实际落库 %d 条", okN, got)
	if failN > 0 {
		t.Fatalf("存在 %d 次失败上报", failN)
	}
	if got < okN {
		t.Fatalf("落库数量不足：期望 %d，实际 %d", okN, got)
	}
}

// TestLoadLatest 只读压测：Latest() 在 1000 台 / 200 万条历史下的耗时。
func TestLoadLatest(t *testing.T) {
	if os.Getenv("SMD_LOAD") != "1" {
		t.Skip("设置 SMD_LOAD=1 后运行负载测试")
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	const nodes, perNode = 1000, 2000
	base := time.Now().Add(-24 * time.Hour).UnixMilli()
	t0 := time.Now()
	buf := make([]store.Record, 0, perNode)
	for i := 0; i < nodes; i++ {
		uuid := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		buf = buf[:0]
		for j := 0; j < perNode; j++ {
			buf = append(buf, store.Record{
				ServerUUID: uuid, ServerName: fmt.Sprintf("node-%03d", i),
				CPU: float64(j % 100), Mem: 50, Source: store.SourceRemote,
				CreatedAt: base + int64(j)*1000,
			})
		}
		if err := st.InsertBatch(buf); err != nil {
			t.Fatalf("灌入数据失败: %v", err)
		}
	}
	t.Logf("[灌数] %d 台 × %d 条 = %d 行，耗时 %v", nodes, perNode, nodes*perNode, time.Since(t0).Round(time.Millisecond))

	for i := 0; i < 3; i++ {
		t1 := time.Now()
		latest, err := st.Latest()
		if err != nil {
			t.Fatalf("Latest 失败: %v", err)
		}
		t.Logf("[Latest] 第 %d 次：%d 台，耗时 %v", i+1, len(latest), time.Since(t1).Round(time.Microsecond))
	}
}

// freePort 在 20000~29999 之间找一个可监听端口。
// 刻意避开系统临时端口段（macOS 为 49152~65535），否则压测自身产生的大量
// 客户端 socket 会把选中的端口占掉，导致 bind: address already in use。
func freePort(t *testing.T) int {
	t.Helper()
	for p := 20000 + int(time.Now().UnixNano()%5000); p < 30000; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			l.Close()
			return p
		}
	}
	t.Fatal("找不到空闲端口")
	return 0
}

func waitPort(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
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

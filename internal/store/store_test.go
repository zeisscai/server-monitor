package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

const (
	uuidA = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	uuidB = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
)

func TestInsertAndLatest(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().Add(-time.Hour).UnixMilli()

	// A：3 条本机记录；B：2 条上报记录
	for i := 0; i < 3; i++ {
		if err := s.Insert(Record{ServerUUID: uuidA, ServerName: "A", CPU: float64(10 * i), Mem: 20, Source: SourceLocal, CreatedAt: base + int64(i)*1000}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.Insert(Record{ServerUUID: uuidB, ServerName: "B", CPU: 30, Mem: float64(40 + i), Source: SourceRemote, CreatedAt: base + int64(i)*1000}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	latest, err := s.Latest()
	if err != nil {
		t.Fatalf("Latest 失败: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("应有 2 台服务器，实际 %d", len(latest))
	}

	byID := map[string]Latest{}
	for _, l := range latest {
		byID[l.ServerUUID] = l
	}
	a := byID[uuidA]
	if a.CPU != 20 || a.Source != SourceLocal || a.Count != 3 {
		t.Fatalf("A 的最新值不正确: %+v", a)
	}
	b := byID[uuidB]
	if b.Mem != 41 || b.Source != SourceRemote || b.Count != 2 {
		t.Fatalf("B 的最新值不正确: %+v", b)
	}
	if latest[0].CreatedAt < latest[1].CreatedAt {
		t.Fatal("Latest 应按更新时间倒序")
	}
}

func TestHistoryRangeAndOrder(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().Add(-time.Hour).UnixMilli()
	for i := 0; i < 10; i++ {
		_ = s.Insert(Record{ServerUUID: uuidA, ServerName: "A", CPU: float64(i), Mem: 1, Source: SourceLocal, CreatedAt: base + int64(i)*1000})
	}

	all, err := s.History(uuidA, base, base+9000, 100)
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(all) != 10 {
		t.Fatalf("应返回 10 条，实际 %d", len(all))
	}
	if all[0].CPU != 9 {
		t.Fatalf("应按时间倒序返回，首条 CPU = %v", all[0].CPU)
	}

	// 限定范围 + 条数上限
	part, err := s.History(uuidA, base, base+4000, 100)
	if err != nil {
		t.Fatalf("History 失败: %v", err)
	}
	if len(part) != 5 {
		t.Fatalf("范围内应有 5 条，实际 %d", len(part))
	}
	limited, _ := s.History(uuidA, base, base+9000, 3)
	if len(limited) != 3 {
		t.Fatalf("limit 未生效，实际 %d", len(limited))
	}
}

func TestSeriesAndStat(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	from := now - 60000

	// 三个 20 秒区间，每区间 2 条
	for i := 0; i < 6; i++ {
		ts := from + int64(i)*10000
		_ = s.Insert(Record{ServerUUID: uuidA, ServerName: "A", CPU: float64(i * 10), Mem: float64(50 - i), Source: SourceLocal, CreatedAt: ts})
	}

	pts, err := s.Series(uuidA, from, now, 3)
	if err != nil {
		t.Fatalf("Series 失败: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("应聚合为 3 个点，实际 %d", len(pts))
	}
	if pts[0].CPU != 5 { // (0 + 10) / 2
		t.Fatalf("首个聚合点 CPU 应为 5，实际 %v", pts[0].CPU)
	}
	if pts[1].CPU != 25 { // (20 + 30) / 2
		t.Fatalf("第二个聚合点 CPU 应为 25，实际 %v", pts[1].CPU)
	}

	st, err := s.Stat(uuidA, from, now)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if st.Count != 6 || st.CPUMax != 50 || st.CPUAvg != 25 {
		t.Fatalf("统计值不正确: %+v", st)
	}

	// 无数据的时间范围应返回空结果而不是报错
	empty, err := s.Series(uuidA, now+100000, now+200000, 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空范围应返回空切片: %v %d", err, len(empty))
	}
	st2, err := s.Stat(uuidA, now+100000, now+200000)
	if err != nil || st2.Count != 0 {
		t.Fatalf("空范围统计应全为 0: %v %+v", err, st2)
	}
}

func TestSizeAndCountAndName(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.Count(); err != nil || n != 0 {
		t.Fatalf("空库计数应为 0: %v %d", err, n)
	}
	_ = s.Insert(Record{ServerUUID: uuidA, ServerName: "第一台", CPU: 1, Mem: 2, Source: SourceLocal, CreatedAt: time.Now().UnixMilli()})
	_ = s.Insert(Record{ServerUUID: uuidA, ServerName: "改名后", CPU: 1, Mem: 2, Source: SourceLocal, CreatedAt: time.Now().UnixMilli() + 1000})

	n, err := s.Count()
	if err != nil || n != 2 {
		t.Fatalf("计数应为 2: %v %d", err, n)
	}
	name, err := s.Name(uuidA)
	if err != nil || name != "改名后" {
		t.Fatalf("应取最近一次的名称: %v %q", err, name)
	}
	name, err = s.Name("cccccccc-3333-4333-8333-cccccccccccc")
	if err != nil || name != "" {
		t.Fatalf("未知服务器应返回空名称: %v %q", err, name)
	}
	size, err := s.Size()
	if err != nil || size <= 0 {
		t.Fatalf("数据库大小应为正数: %v %d", err, size)
	}
}

func TestInsertDefaults(t *testing.T) {
	s := newTestStore(t)
	// CreatedAt 为 0 时应自动补当前时间；Source 为空时默认本机
	if err := s.Insert(Record{ServerUUID: uuidA, ServerName: "A", CPU: 1, Mem: 2}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	latest, err := s.Latest()
	if err != nil || len(latest) != 1 {
		t.Fatalf("Latest 失败: %v", err)
	}
	if latest[0].Source != SourceLocal {
		t.Fatalf("默认来源应为本机，实际 %q", latest[0].Source)
	}
	if time.Since(time.UnixMilli(latest[0].CreatedAt)) > time.Minute {
		t.Fatal("未自动补充采集时间")
	}
}

func TestOpenRejectsCorruptDatabase(t *testing.T) {
	dir := t.TempDir()
	// 写入一个非 SQLite 内容的文件，Open 应当在两种日志模式下都失败，
	// 并给出带路径的错误信息（便于定位）。
	if err := os.WriteFile(filepath.Join(dir, "monitor.db"), []byte("this is not a sqlite database"), 0o644); err != nil {
		t.Fatalf("准备损坏文件失败: %v", err)
	}
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatal("损坏的数据库文件应当返回错误")
	}
	if !strings.Contains(err.Error(), "monitor.db") {
		t.Fatalf("错误信息应包含数据库路径，实际: %v", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if err := s.Insert(Record{ServerUUID: uuidA, ServerName: "A", CPU: 5, Mem: 6, Source: SourceLocal, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer s2.Close()
	n, err := s2.Count()
	if err != nil || n != 1 {
		t.Fatalf("重开后数据应保留: %v %d", err, n)
	}
}

// 并发写入：多台服务器同时上报时，记录不能丢、计数不能错。
func TestConcurrentInserts(t *testing.T) {
	s := newTestStore(t)
	const workers, per = 200, 25

	var wg sync.WaitGroup
	errCh := make(chan error, workers*per)
	base := time.Now().Add(-time.Minute).UnixMilli()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			uuid := fmt.Sprintf("dddddddd-0000-4000-8000-%012d", w)
			for i := 0; i < per; i++ {
				err := s.Insert(Record{
					ServerUUID: uuid, ServerName: "并发",
					CPU: float64(i), Mem: 50, Source: SourceRemote,
					CreatedAt: base + int64(i)*1000,
				})
				if err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发写入失败: %v", err)
	}

	n, err := s.Count()
	if err != nil || n != workers*per {
		t.Fatalf("应有 %d 条记录，实际 %d（%v）", workers*per, n, err)
	}
	latest, err := s.Latest()
	if err != nil || len(latest) != workers {
		t.Fatalf("应有 %d 台服务器，实际 %d（%v）", workers, len(latest), err)
	}
	for _, l := range latest {
		if l.Count != per {
			t.Fatalf("%s 计数应为 %d，实际 %d", l.ServerUUID, per, l.Count)
		}
		if l.CPU != float64(per-1) {
			t.Fatalf("%s 最新一条应取时间最大的记录，实际 CPU=%v", l.ServerUUID, l.CPU)
		}
	}
}

// 乱序上报：更旧的记录不应把面板上的最新值改回去，但计数要照常累加。
func TestOutOfOrderReports(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.Insert(Record{ServerUUID: uuidA, ServerName: "新", CPU: 80, Mem: 70, Source: SourceRemote, CreatedAt: now}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := s.Insert(Record{ServerUUID: uuidA, ServerName: "旧", CPU: 10, Mem: 20, Source: SourceRemote, CreatedAt: now - 5000}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	latest, err := s.Latest()
	if err != nil || len(latest) != 1 {
		t.Fatalf("Latest 失败: %v %d", err, len(latest))
	}
	if latest[0].CPU != 80 || latest[0].ServerName != "新" || latest[0].CreatedAt != now {
		t.Fatalf("旧记录不应覆盖最新值: %+v", latest[0])
	}
	if latest[0].Count != 2 {
		t.Fatalf("计数应包含乱序记录，实际 %d", latest[0].Count)
	}
}

// 老版本升级：历史表已有数据、物化表为空时，Open 应自动回填最新值。
func TestBackfillLatestOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor.db")

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("打开老库失败: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE records (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			server_uuid TEXT    NOT NULL,
			server_name TEXT    NOT NULL,
			cpu_usage   REAL    NOT NULL,
			mem_usage   REAL    NOT NULL,
			source      TEXT    NOT NULL,
			created_at  INTEGER NOT NULL
		);
		CREATE INDEX idx_records_uuid_time ON records (server_uuid, created_at);
		CREATE INDEX idx_records_time      ON records (created_at);`); err != nil {
		t.Fatalf("建老表失败: %v", err)
	}
	base := time.Now().Add(-time.Hour).UnixMilli()
	rows := [][4]any{
		{uuidA, "A-旧名", 10.0, base},
		{uuidA, "A-新名", 90.0, base + 1000},
		{uuidA, "A-中间", 50.0, base + 500},
		{uuidB, "B", 30.0, base + 2000},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO records (server_uuid, server_name, cpu_usage, mem_usage, source, created_at)
			 VALUES (?, ?, ?, 40, 'remote', ?)`, r[0], r[1], r[2], r[3],
		); err != nil {
			t.Fatalf("写老数据失败: %v", err)
		}
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级打开失败: %v", err)
	}
	defer s.Close()

	latest, err := s.Latest()
	if err != nil || len(latest) != 2 {
		t.Fatalf("回填后应有 2 台服务器: %v %d", err, len(latest))
	}
	byID := map[string]Latest{}
	for _, l := range latest {
		byID[l.ServerUUID] = l
	}
	if a := byID[uuidA]; a.Count != 3 || a.CPU != 90 || a.ServerName != "A-新名" {
		t.Fatalf("A 的回填结果不正确: %+v", a)
	}
	if b := byID[uuidB]; b.Count != 1 || b.CPU != 30 {
		t.Fatalf("B 的回填结果不正确: %+v", b)
	}
	if n, _ := s.Count(); n != 4 {
		t.Fatalf("总记录数应为 4，实际 %d", n)
	}
}

// 关闭后再写入应返回 ErrClosed，而不是 panic 或阻塞。
func TestInsertAfterClose(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if err := s.Insert(Record{ServerUUID: uuidA, ServerName: "A", CPU: 1, Mem: 2}); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后写入应返回 ErrClosed，实际 %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("重复关闭不应报错: %v", err)
	}
}

// Package store 封装 SQLite 存储：建表、写入采集记录、按服务器取最新值、
// 按时间范围查询历史与聚合曲线，以及统计数据库占用空间。
//
// 所有历史数据永久保留，不做任何清理。
//
// 并发模型（面向「千台服务器同时上报」）：
//
//	HTTP 上报 ─Insert─▶ 写入队列(16384) ─▶ 单写线程 ─▶ 批量事务(≤512 条 / 10ms)
//	                                          └─▶ server_latest 物化表（面板只读这 1000 行）
//
// 几个要点：
//
//  1. 所有写入由唯一一个写线程串行执行，天然满足 SQLite 的单写者约束，没有锁竞争。
//  2. 写线程把队列里的记录攒成一批，一次 BEGIN/COMMIT 提交，从「每条一次提交」
//     变成「每批一次提交」；同批次内同一台服务器只保留最新一条。
//  3. 读查询走连接池里的其它连接（WAL 允许一写多读），面板刷新不再阻塞上报写入。
//  4. 最新值单独维护在 server_latest 表，面板查询不再对历史表做 JOIN + 相关子查询，
//     开销与历史数据量无关（旧实现 1000 台 / 200 万条时单次查询约 500ms）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，便于 CGO_ENABLED=0 交叉编译
)

// 数据来源
const (
	SourceLocal  = "local"  // 本机采集
	SourceRemote = "remote" // 其他服务器上报
)

// 写入队列与批量提交参数。
//
// 队列容量按「1000 台 × 2 秒一次 = 500 条/秒」估算，16384 条约可吸收 30 秒的突发，
// 队列满时 Insert 会阻塞（调用方是 HTTP handler，天然形成背压），不会丢数据。
const (
	writeQueueSize = 16384
	maxWriteBatch  = 512
	flushWindow    = 3 * time.Millisecond
)

// ErrClosed 表示存储已关闭，新的写入会被拒绝。
var ErrClosed = errors.New("存储已关闭")

// Record 是一条采集记录。
type Record struct {
	ServerUUID string  `json:"uuid"`
	ServerName string  `json:"name"`
	CPU        float64 `json:"cpu"`
	Mem        float64 `json:"mem"`
	Source     string  `json:"source"`
	CreatedAt  int64   `json:"ts"` // Unix 毫秒
}

// Latest 是某台服务器的最新状态。
type Latest struct {
	ServerUUID string  `json:"uuid"`
	ServerName string  `json:"name"`
	CPU        float64 `json:"cpu"`
	Mem        float64 `json:"mem"`
	Source     string  `json:"source"`
	CreatedAt  int64   `json:"ts"`
	Count      int64   `json:"count"`
}

// Point 是曲线上的一个聚合点。
type Point struct {
	T   int64   `json:"t"`
	CPU float64 `json:"cpu"`
	Mem float64 `json:"mem"`
}

// Stats 是时间范围内的统计值。
type Stats struct {
	Count  int64   `json:"count"`
	CPUAvg float64 `json:"cpu_avg"`
	CPUMax float64 `json:"cpu_max"`
	MemAvg float64 `json:"mem_avg"`
	MemMax float64 `json:"mem_max"`
}

// Store 是数据库句柄。
type Store struct {
	db   *sql.DB
	path string

	// 写线程
	ch     chan writeOp
	done   chan struct{}
	wconn  *sql.Conn
	ins    *sql.Stmt
	latest *sql.Stmt
	incr   *sql.Stmt

	mu     sync.RWMutex // 保护 closed 与入队，避免向已关闭的 channel 发送
	closed bool

	flushMu sync.Mutex // 串行化批提交（写线程与 InsertBatch 共用）

	batch   []writeOp
	agg     map[string]*aggEntry
	aggKeys []string
}

type writeOp struct {
	rec  Record
	done chan error
}

type aggEntry struct {
	rec Record
	n   int64
}

const schema = `
CREATE TABLE IF NOT EXISTS records (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	server_uuid TEXT    NOT NULL,
	server_name TEXT    NOT NULL,
	cpu_usage   REAL    NOT NULL,
	mem_usage   REAL    NOT NULL,
	source      TEXT    NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_records_uuid_time ON records (server_uuid, created_at);
CREATE INDEX IF NOT EXISTS idx_records_time      ON records (created_at);
CREATE TABLE IF NOT EXISTS server_latest (
	server_uuid TEXT    PRIMARY KEY,
	server_name TEXT    NOT NULL,
	cpu_usage   REAL    NOT NULL,
	mem_usage   REAL    NOT NULL,
	source      TEXT    NOT NULL,
	created_at  INTEGER NOT NULL,
	cnt         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_latest_time ON server_latest (created_at);
`

// Open 打开（必要时创建）数据库文件并初始化表结构。
//
// 优先启用 WAL + busy_timeout；部分文件系统（容器 overlayfs、网络盘、部分云盘）
// 不支持 WAL，此时自动回退到默认日志模式，而不是直接启动失败。
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("数据目录不能为空")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败 %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, "monitor.db")

	db, err := openWithDSN("file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-16000)&_pragma=temp_store(MEMORY)")
	if err != nil {
		// WAL 不可用时退回默认日志模式重试一次。
		db, err = openWithDSN("file:" + path + "?_pragma=busy_timeout(10000)")
		if err != nil {
			return nil, fmt.Errorf("初始化数据库失败 %s: %w", path, err)
		}
	}

	s := &Store{
		db:   db,
		path: path,
		ch:   make(chan writeOp, writeQueueSize),
		done: make(chan struct{}),
		agg:  make(map[string]*aggEntry, maxWriteBatch),
	}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	go s.writer()
	return s, nil
}

func openWithDSN(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 写入由单写线程独占一条连接；其余连接用于读，WAL 下读写互不阻塞。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// init 准备写连接与预编译语句，并把老库的最新值回填到物化表。
func (s *Store) init() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取写连接失败: %w", err)
	}
	s.wconn = conn

	s.ins, err = conn.PrepareContext(ctx, `
		INSERT INTO records (server_uuid, server_name, cpu_usage, mem_usage, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("准备写入语句失败: %w", err)
	}
	// 只有更新的记录才覆盖最新值，避免乱序上报让面板「倒退」。
	// cnt 固定写 0，再由随后的 UPDATE 累加，保证乱序时计数依然精确。
	s.latest, err = conn.PrepareContext(ctx, `
		INSERT INTO server_latest (server_uuid, server_name, cpu_usage, mem_usage, source, created_at, cnt)
		VALUES (?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(server_uuid) DO UPDATE SET
			server_name = excluded.server_name,
			cpu_usage   = excluded.cpu_usage,
			mem_usage   = excluded.mem_usage,
			source      = excluded.source,
			created_at  = excluded.created_at
		WHERE excluded.created_at >= server_latest.created_at`)
	if err != nil {
		return fmt.Errorf("准备最新值语句失败: %w", err)
	}
	s.incr, err = conn.PrepareContext(ctx, `UPDATE server_latest SET cnt = cnt + ? WHERE server_uuid = ?`)
	if err != nil {
		return fmt.Errorf("准备计数语句失败: %w", err)
	}
	return s.backfillLatest()
}

// backfillLatest 在 server_latest 为空但历史表已有数据时（老版本升级）做一次性回填。
func (s *Store) backfillLatest() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM server_latest`).Scan(&n); err != nil {
		return fmt.Errorf("查询最新值表失败: %w", err)
	}
	if n > 0 {
		return nil
	}
	var has int
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM records)`).Scan(&has); err != nil {
		return fmt.Errorf("查询历史表失败: %w", err)
	}
	if has == 0 {
		return nil
	}
	if _, err := s.db.Exec(`
		INSERT OR REPLACE INTO server_latest (server_uuid, server_name, cpu_usage, mem_usage, source, created_at, cnt)
		SELECT r.server_uuid, r.server_name, r.cpu_usage, r.mem_usage, r.source, r.created_at,
		       (SELECT COUNT(*) FROM records c WHERE c.server_uuid = r.server_uuid)
		FROM records r
		JOIN (
			SELECT server_uuid, MAX(created_at) AS mx
			FROM records GROUP BY server_uuid
		) m ON m.server_uuid = r.server_uuid AND m.mx = r.created_at
		GROUP BY r.server_uuid`); err != nil {
		return fmt.Errorf("回填最新值表失败: %w", err)
	}
	return nil
}

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Close 停止写线程，把队列中已接收的记录全部落盘后再关闭数据库。
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()

	<-s.done // 写线程会把剩余记录刷完再退出
	if s.wconn != nil {
		_ = s.wconn.Close()
	}
	return s.db.Close()
}

// Insert 写入一条记录，等这批数据真正提交后返回。
//
// 单条写入会被写线程合并进同一批次，因此调用开销远小于「一条一次提交」。
func (s *Store) Insert(r Record) error {
	done := make(chan error, 1)
	if err := s.enqueue(writeOp{rec: r, done: done}); err != nil {
		return err
	}
	return <-done
}

// InsertBatch 同步批量写入（用于灌历史数据 / 迁移），不与写线程并发提交。
func (s *Store) InsertBatch(rows []Record) error {
	if len(rows) == 0 {
		return nil
	}
	ops := make([]writeOp, len(rows))
	for i := range rows {
		ops[i].rec = rows[i]
	}
	return s.flush(ops)
}

func (s *Store) enqueue(op writeOp) error {
	op.rec = normalize(op.rec)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	// 队列满时阻塞在此处，形成背压而不是丢数据。
	// Close 需要拿到写锁，因此不会在仍有发送者卡住时关闭 channel。
	s.ch <- op
	return nil
}

// writer 是唯一的写入者：把队列攒成批次提交，批次完成后唤醒等待的调用方。
func (s *Store) writer() {
	defer close(s.done)
	timer := time.NewTimer(flushWindow)
	timer.Stop()

	var drained bool
	for {
		op, ok := <-s.ch
		if !ok {
			return
		}
		batch := append(s.batch[:0], op)
		deadline := time.Now().Add(flushWindow)

	fill:
		for len(batch) < maxWriteBatch {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			timer.Reset(remaining)
			select {
			case o, ok2 := <-s.ch:
				timer.Stop()
				if !ok2 {
					drained = true
					break fill
				}
				batch = append(batch, o)
			case <-timer.C:
				break fill
			}
		}

		err := s.flush(batch)
		for i := range batch {
			if batch[i].done != nil {
				batch[i].done <- err
			}
		}
		for i := range batch {
			batch[i] = writeOp{}
		}
		s.batch = batch[:0]

		if drained {
			return
		}
	}
}

// flush 在一个事务里写入整批记录，并维护 server_latest。
func (s *Store) flush(batch []writeOp) error {
	if len(batch) == 0 {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	// 同批次内按 uuid 聚合：latest 只写一条（取时间最新的），计数累加。
	keys := s.aggKeys[:0]
	for i := range batch {
		r := batch[i].rec
		e := s.agg[r.ServerUUID]
		if e == nil {
			e = &aggEntry{}
			s.agg[r.ServerUUID] = e
			keys = append(keys, r.ServerUUID)
		}
		e.n++
		if e.rec.CreatedAt == 0 || r.CreatedAt >= e.rec.CreatedAt {
			e.rec = r
		}
	}

	err := s.commit(batch, keys)
	for _, k := range keys {
		delete(s.agg, k)
	}
	s.aggKeys = keys[:0]
	return err
}

func (s *Store) commit(batch []writeOp, keys []string) error {
	ctx := context.Background()
	if _, err := s.wconn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	for i := range batch {
		r := batch[i].rec
		if _, err := s.ins.ExecContext(ctx, r.ServerUUID, r.ServerName, r.CPU, r.Mem, r.Source, r.CreatedAt); err != nil {
			_, _ = s.wconn.ExecContext(ctx, "ROLLBACK")
			return err
		}
	}
	for _, k := range keys {
		e := s.agg[k]
		if _, err := s.latest.ExecContext(ctx, k, e.rec.ServerName, e.rec.CPU, e.rec.Mem, e.rec.Source, e.rec.CreatedAt); err != nil {
			_, _ = s.wconn.ExecContext(ctx, "ROLLBACK")
			return err
		}
		if _, err := s.incr.ExecContext(ctx, e.n, k); err != nil {
			_, _ = s.wconn.ExecContext(ctx, "ROLLBACK")
			return err
		}
	}
	if _, err := s.wconn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = s.wconn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	return nil
}

func normalize(r Record) Record {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}
	if r.Source == "" {
		r.Source = SourceLocal
	}
	return r
}

// Latest 返回每台服务器的最新一条记录及历史记录总数。
//
// 只扫描 server_latest（每台服务器一行），耗时与历史数据量无关。
func (s *Store) Latest() ([]Latest, error) {
	rows, err := s.db.Query(`
		SELECT server_uuid, server_name, cpu_usage, mem_usage, source, created_at, cnt
		FROM server_latest
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Latest{}
	for rows.Next() {
		var l Latest
		if err := rows.Scan(&l.ServerUUID, &l.ServerName, &l.CPU, &l.Mem, &l.Source, &l.CreatedAt, &l.Count); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Name 返回某台服务器最近使用的名称。
func (s *Store) Name(uuid string) (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT server_name FROM server_latest WHERE server_uuid = ?`, uuid).Scan(&name)
	if err == nil {
		return name, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	// 兜底：物化表尚未包含该服务器（例如从极老版本直接升级）。
	err = s.db.QueryRow(
		`SELECT server_name FROM records WHERE server_uuid = ? ORDER BY created_at DESC LIMIT 1`, uuid,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// History 返回时间范围内的历史记录（按时间正序，最多 limit 条）。
func (s *Store) History(uuid string, from, to int64, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
		SELECT server_uuid, server_name, cpu_usage, mem_usage, source, created_at
		FROM records
		WHERE server_uuid = ? AND created_at BETWEEN ? AND ?
		ORDER BY created_at DESC
		LIMIT ?`, uuid, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ServerUUID, &r.ServerName, &r.CPU, &r.Mem, &r.Source, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Series 把时间范围等分为 buckets 个区间并求均值，用于绘制曲线（避免前端拉取海量原始点）。
func (s *Store) Series(uuid string, from, to int64, buckets int) ([]Point, error) {
	if buckets <= 0 {
		buckets = 720
	}
	if to <= from {
		return []Point{}, nil
	}
	span := to - from
	width := span / int64(buckets)
	if width < 1 {
		width = 1
	}
	rows, err := s.db.Query(`
		SELECT (created_at - ?) / ? AS bucket,
		       CAST(AVG(created_at) AS INTEGER), AVG(cpu_usage), AVG(mem_usage)
		FROM records
		WHERE server_uuid = ? AND created_at BETWEEN ? AND ?
		GROUP BY bucket
		ORDER BY bucket`, from, width, uuid, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Point{}
	for rows.Next() {
		var b int64
		var p Point
		if err := rows.Scan(&b, &p.T, &p.CPU, &p.Mem); err != nil {
			return nil, err
		}
		p.CPU = round1(p.CPU)
		p.Mem = round1(p.Mem)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Stat 返回时间范围内的统计值。
func (s *Store) Stat(uuid string, from, to int64) (Stats, error) {
	var st Stats
	var cpuAvg, cpuMax, memAvg, memMax sql.NullFloat64
	err := s.db.QueryRow(`
		SELECT COUNT(*), AVG(cpu_usage), MAX(cpu_usage), AVG(mem_usage), MAX(mem_usage)
		FROM records
		WHERE server_uuid = ? AND created_at BETWEEN ? AND ?`, uuid, from, to,
	).Scan(&st.Count, &cpuAvg, &cpuMax, &memAvg, &memMax)
	if err != nil {
		return st, err
	}
	st.CPUAvg = round1(cpuAvg.Float64)
	st.CPUMax = round1(cpuMax.Float64)
	st.MemAvg = round1(memAvg.Float64)
	st.MemMax = round1(memMax.Float64)
	return st, nil
}

// Size 返回数据库占用的空间大小（字节），包含 WAL / SHM 附属文件。
func (s *Store) Size() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		fi, err := os.Stat(s.path + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return total, err
		}
		total += fi.Size()
	}
	return total, nil
}

// Count 返回记录总数（由 server_latest.cnt 汇总，避免全表扫描）。
func (s *Store) Count() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cnt), 0) FROM server_latest`).Scan(&n)
	return n, err
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

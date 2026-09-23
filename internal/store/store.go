// Package store 封装 SQLite 存储：建表、写入采集记录、按服务器取最新值、
// 按时间范围查询历史与聚合曲线，以及统计数据库占用空间。
//
// 所有历史数据永久保留，不做任何清理。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，便于 CGO_ENABLED=0 交叉编译
)

// 数据来源
const (
	SourceLocal  = "local"  // 本机采集
	SourceRemote = "remote" // 其他服务器上报
)

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

	db, err := openWithDSN("file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		// WAL 不可用时退回默认日志模式重试一次。
		db, retryErr := openWithDSN("file:" + path + "?_pragma=busy_timeout(10000)")
		if retryErr != nil {
			return nil, fmt.Errorf("初始化数据库失败 %s: %w", path, retryErr)
		}
		return &Store{db: db, path: path}, nil
	}
	return &Store{db: db, path: path}, nil
}

func openWithDSN(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 采集写入量极小但要求绝不丢数据，串行化访问可彻底避免锁竞争。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Insert 写入一条采集记录。
func (s *Store) Insert(r Record) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}
	if r.Source == "" {
		r.Source = SourceLocal
	}
	_, err := s.db.Exec(
		`INSERT INTO records (server_uuid, server_name, cpu_usage, mem_usage, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.ServerUUID, r.ServerName, r.CPU, r.Mem, r.Source, r.CreatedAt,
	)
	return err
}

// Latest 返回每台服务器的最新一条记录及历史记录总数。
func (s *Store) Latest() ([]Latest, error) {
	rows, err := s.db.Query(`
		SELECT r.server_uuid, r.server_name, r.cpu_usage, r.mem_usage, r.source, r.created_at,
		       (SELECT COUNT(*) FROM records c WHERE c.server_uuid = r.server_uuid)
		FROM records r
		JOIN (
			SELECT server_uuid, MAX(created_at) AS mx
			FROM records GROUP BY server_uuid
		) m ON m.server_uuid = r.server_uuid AND m.mx = r.created_at
		GROUP BY r.server_uuid
		ORDER BY r.created_at DESC`)
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
	err := s.db.QueryRow(
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

// Count 返回记录总数。
func (s *Store) Count() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM records`).Scan(&n)
	return n, err
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

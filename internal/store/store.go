// store.go — ClickHouse 写入层。批写 + 本地 WAL（jsonl，50 MB 环形）。
package store

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

const (
	walMaxBytes = 50 << 20 // 50 MB
	defaultDSN  = "clickhouse://localhost:9000/nodedata?dial_timeout=5s"
)

// Row 对应 samples 表的一行。
type Row struct {
	Host     string
	MetricID string
	TS       time.Time
	Value    float64
	Z        []int8 // 14 档，Int8×20，null 用 0 表示
}

// Store 管理 ClickHouse 写入与 WAL。
type Store struct {
	db     *sql.DB
	mu     sync.Mutex
	wal    *os.File
	walSz  int64
	walDir string
	host   string
}

// New 创建 Store。dsn 为空时用默认值。
func New(dsn, walDir, host string) (*Store, error) {
	if dsn == "" {
		dsn = defaultDSN
	}
	if walDir == "" {
		walDir = "."
	}
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	s := &Store{db: db, walDir: walDir, host: host}
	if err := s.openWAL(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) openWAL() error {
	path := filepath.Join(s.walDir, "nodedata-wal.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	s.wal = f
	s.walSz = st.Size()
	return nil
}

// WriteBatch 批量写入一组行，失败时写 WAL。
func (s *Store) WriteBatch(rows []Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.insertBatch(rows); err != nil {
		s.walFallback(rows)
		return fmt.Errorf("clickhouse insert failed, wrote WAL: %w", err)
	}
	return nil
}

func (s *Store) insertBatch(rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO samples (host, metric_id, ts, value, z) VALUES (?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(r.Host, r.MetricID, r.TS.UTC(), r.Value, r.Z); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) walFallback(rows []Row) {
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		b = append(b, '\n')
		needed := int64(len(b))
		// 环形：超出上限则截断（丢最旧）
		if s.walSz+needed > walMaxBytes {
			// 重新创建（丢旧数据）
			s.wal.Close()
			path := filepath.Join(s.walDir, "nodedata-wal.jsonl")
			os.Remove(path)
			s.openWAL()
		}
		n, _ := s.wal.Write(b)
		s.walSz += int64(n)
	}
}

// ReplayWAL 重放 WAL 到 ClickHouse，成功后清空文件。
func (s *Store) ReplayWAL() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.walDir, "nodedata-wal.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	var rows []Row
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r Row
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.insertBatch(rows); err != nil {
		return err
	}
	// 清空 WAL
	f.Close()
	os.Remove(path)
	s.wal.Close()
	return s.openWAL()
}

// QueryPrevValues 为 14 个时间戳批量点查 v(t-H)。
// 返回 map[lagIndex]value，找不到的档位不在 map 里。
func (s *Store) QueryPrevValues(host, metricID string, targets []time.Time, tol time.Duration) map[int]float64 {
	result := make(map[int]float64, len(targets))
	if len(targets) == 0 {
		return result
	}

	// 构造 IN 查询
	from := targets[0]
	to := targets[0]
	for _, t := range targets {
		if t.Before(from) {
			from = t
		}
		if t.After(to) {
			to = t
		}
	}
	from = from.Add(-tol)
	to = to.Add(tol)

	rows, err := s.db.Query(
		`SELECT ts, value FROM samples
		 WHERE host=? AND metric_id=? AND ts BETWEEN ? AND ?
		 ORDER BY ts`,
		host, metricID, from.UTC(), to.UTC())
	if err != nil {
		return result
	}
	defer rows.Close()

	type pt struct {
		ts  time.Time
		val float64
	}
	var pts []pt
	for rows.Next() {
		var ts time.Time
		var val float64
		if err := rows.Scan(&ts, &val); err != nil {
			continue
		}
		pts = append(pts, pt{ts, val})
	}

	for i, target := range targets {
		best := -1
		bestDist := tol + 1
		for j, p := range pts {
			d := p.ts.Sub(target)
			if d < 0 {
				d = -d
			}
			if d < bestDist {
				bestDist = d
				best = j
			}
		}
		if best >= 0 && bestDist <= tol {
			result[i] = pts[best].val
		}
	}
	return result
}

// WatchdogEvict 检查 ClickHouse 磁盘占用，超出 900 MB 时删最旧日分区直至 800 MB 以下。
func (s *Store) WatchdogEvict() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var total int64
	row := s.db.QueryRow(
		`SELECT sum(bytes_on_disk) FROM system.parts WHERE table='samples' AND active=1`)
	if err := row.Scan(&total); err != nil {
		return err
	}
	if total <= 900<<20 {
		return nil
	}
	// 找最旧分区
	for total > 800<<20 {
		var part string
		row = s.db.QueryRow(
			`SELECT partition FROM system.parts WHERE table='samples' AND active=1 ORDER BY partition LIMIT 1`)
		if err := row.Scan(&part); err != nil {
			return err
		}
		if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE samples DROP PARTITION '%s'", part)); err != nil {
			return err
		}
		row = s.db.QueryRow(
			`SELECT sum(bytes_on_disk) FROM system.parts WHERE table='samples' AND active=1`)
		if err := row.Scan(&total); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s.wal != nil {
		s.wal.Close()
	}
	return s.db.Close()
}

// Ping 测试 ClickHouse 连通性。
func (s *Store) Ping() error {
	return s.db.Ping()
}

// ReplayWALN 重放 WAL 并返回重放行数。
func (s *Store) ReplayWALN() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.walDir, "nodedata-wal.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var rows []Row
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r Row
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := s.insertBatch(rows); err != nil {
		return 0, err
	}
	f.Close()
	os.Remove(path)
	s.wal.Close()
	newF, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return len(rows), err
	}
	s.wal = newF
	s.walSz = 0
	return len(rows), nil
}

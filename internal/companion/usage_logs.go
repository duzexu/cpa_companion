package companion

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type usageLogStore struct{ db *sql.DB }

const usageLogSelectColumns = `id,at_ms,user_id,username,key_id,plan_id,account_id,account_prefix,model,route,status,duration_ms,first_byte_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,charge_pico_usd,estimated,error_code,error_message`

type usageLog struct {
	ID                                                                          int64
	At                                                                          time.Time
	UserID, KeyID, PlanID, AccountID, AccountPrefix, Model, Route               string
	Username                                                                    string // Snapshot of the username when the request starts.
	ErrorCode, ErrorMessage                                                     string
	Status, DurationMS                                                          int
	FirstByteMS                                                                 *int
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens, ChargePicoUSD int64
	Estimated                                                                   bool
	AuditIDs                                                                    []int64
}

func (l usageLog) CostUSD() string   { return formatPicoUSD(l.ChargePicoUSD) }
func (l usageLog) AtText() string    { return l.At.In(beijing).Format("2006-01-02 15:04:05") }
func (l usageLog) TokenTotal() int64 { return l.InputTokens + l.OutputTokens }
func (l usageLog) Successful() bool  { return l.Status >= 200 && l.Status < 300 }
func (l usageLog) CacheHitRateText() string {
	if l.InputTokens <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(l.CacheReadTokens)/float64(l.InputTokens))
}
func (l usageLog) ErrorCodeText() string {
	if l.ErrorCode != "" {
		return l.ErrorCode
	}
	return "HTTP " + strconv.Itoa(l.Status)
}
func (l usageLog) FirstByteText() string {
	if l.FirstByteMS == nil {
		return "—"
	}
	return fmt.Sprintf("%d ms", *l.FirstByteMS)
}

type usageFilter struct {
	ID                                     int64
	UserID, Model, KeyID, AccountID, Route string
	Status                                 string
	Since                                  time.Time
	Until                                  time.Time
	Page                                   int
	Limit                                  int
}

type usageSummary struct {
	Requests, Successful, InputTokens, OutputTokens, ChargePicoUSD int64
}

func (s usageSummary) CostUSD() string    { return formatPicoUSD(s.ChargePicoUSD) }
func (s usageSummary) TotalTokens() int64 { return s.InputTokens + s.OutputTokens }
func (s usageSummary) SuccessPercent() int {
	if s.Requests == 0 {
		return 0
	}
	return int(s.Successful * 100 / s.Requests)
}

type trafficBucket struct {
	Label                       string
	Success, Failed             int64
	SuccessHeight, FailedHeight int
}

type accountTraffic struct {
	AccountID, Prefix string
	Requests, Success int64
	Points            string
	Buckets           []int64
}

func (v accountTraffic) SuccessPercent() int64 {
	if v.Requests == 0 {
		return 0
	}
	return v.Success * 100 / v.Requests
}

func (s *usageLogStore) traffic(ctx context.Context, userID string, now time.Time) ([]trafficBucket, []accountTraffic, error) {
	const bucketCount = 37 // Partial buckets at both ends of a rolling six-hour window.
	since := now.Add(-6 * time.Hour)
	start := since.Truncate(10 * time.Minute)
	buckets := make([]trafficBucket, bucketCount)
	for i := range buckets {
		buckets[i].Label = start.Add(time.Duration(i) * 10 * time.Minute).In(beijing).Format("15:04")
	}
	query := `SELECT ((at_ms / 600000) * 600000),account_id,account_prefix,
	 COUNT(*),COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),0)
	 FROM usage_logs WHERE at_ms >= ? AND at_ms <= ?`
	args := []any{since.UnixMilli(), now.UnixMilli()}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	query += ` GROUP BY 1,2,3 ORDER BY 1`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byAccount := map[string]*accountTraffic{}
	series := map[string][]int64{}
	for rows.Next() {
		var at int64
		var id, prefix string
		var total, success int64
		if err := rows.Scan(&at, &id, &prefix, &total, &success); err != nil {
			return nil, nil, err
		}
		index := int((at - start.UnixMilli()) / int64(10*time.Minute/time.Millisecond))
		if index < 0 || index >= bucketCount {
			continue
		}
		buckets[index].Success += success
		buckets[index].Failed += total - success
		key := id + "\x00" + prefix
		if byAccount[key] == nil {
			byAccount[key] = &accountTraffic{AccountID: id, Prefix: prefix}
			series[key] = make([]int64, bucketCount)
		}
		byAccount[key].Requests += total
		byAccount[key].Success += success
		series[key][index] += total
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	max := int64(1)
	for _, b := range buckets {
		if b.Success+b.Failed > max {
			max = b.Success + b.Failed
		}
	}
	for i := range buckets {
		buckets[i].SuccessHeight = int(buckets[i].Success * 100 / max)
		buckets[i].FailedHeight = int(buckets[i].Failed * 100 / max)
	}
	accounts := make([]accountTraffic, 0, len(byAccount))
	for key, v := range byAccount {
		v.Buckets = series[key]
		v.Points = sparklinePoints(v.Buckets)
		accounts = append(accounts, *v)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Requests > accounts[j].Requests })
	return buckets, accounts, nil
}

func (s *usageLogStore) distinctModels(ctx context.Context, f usageFilter) (int64, error) {
	where, args := usageWhere(f)
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT model) FROM usage_logs WHERE `+where+` AND model <> ''`, args...).Scan(&count)
	return count, err
}

func sparklinePoints(values []int64) string {
	max := int64(1)
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	points := make([]string, 0, len(values))
	for i, v := range values {
		points = append(points, fmt.Sprintf("%d,%d", i*100/(len(values)-1), 24-v*20/max))
	}
	return strings.Join(points, " ")
}

func openUsageLogs(dataDir string) (*usageLogStore, error) {
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "usage.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at_ms INTEGER NOT NULL,
    user_id TEXT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    key_id TEXT NOT NULL DEFAULT '',
    plan_id TEXT NOT NULL DEFAULT '',
    account_id TEXT NOT NULL DEFAULT '',
    account_prefix TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    route TEXT NOT NULL,
    status INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    first_byte_ms INTEGER,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    charge_pico_usd INTEGER NOT NULL DEFAULT 0,
    estimated INTEGER NOT NULL DEFAULT 0,
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT ''
  )`,
		`CREATE INDEX IF NOT EXISTS usage_logs_user_time ON usage_logs(user_id, at_ms DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS usage_logs_time ON usage_logs(at_ms DESC, id DESC)`,
		`CREATE TABLE IF NOT EXISTS content_audit_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at_ms INTEGER NOT NULL,
    user_id TEXT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    key_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    model TEXT NOT NULL,
    route TEXT NOT NULL,
    first_in_session INTEGER NOT NULL,
    hit INTEGER NOT NULL,
    fallback INTEGER NOT NULL,
    category TEXT NOT NULL,
    score_percent REAL NOT NULL,
    threshold_percent INTEGER NOT NULL,
    input_text TEXT NOT NULL,
    duration_ms INTEGER NOT NULL
  )`,
		`CREATE INDEX IF NOT EXISTS content_audit_records_time ON content_audit_records(at_ms DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS content_audit_records_user_time ON content_audit_records(user_id, at_ms DESC, id DESC)`,
		`CREATE TABLE IF NOT EXISTS audit_record_details (
    audit_id INTEGER PRIMARY KEY,
    usage_log_id INTEGER,
    endpoint_name TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0
  )`,
		`CREATE INDEX IF NOT EXISTS audit_record_details_usage ON audit_record_details(usage_log_id)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize usage database: %w", err)
		}
	}
	if err := os.Chmod(filepath.Join(dataDir, "usage.sqlite"), 0600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure usage database: %w", err)
	}
	return &usageLogStore{db: db}, nil
}
func (s *usageLogStore) Close() error { return s.db.Close() }

func (s *usageLogStore) insert(ctx context.Context, l usageLog) error {
	if l.UserID == "" || l.Status < 100 || l.Status > 599 {
		return errors.New("invalid usage log")
	}
	if l.Username == "" {
		l.Username = l.UserID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO usage_logs
	 (at_ms,user_id,username,key_id,plan_id,account_id,account_prefix,model,route,status,duration_ms,first_byte_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,charge_pico_usd,estimated,error_code,error_message)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.At.UnixMilli(), l.UserID, l.Username, l.KeyID, l.PlanID, l.AccountID, l.AccountPrefix, l.Model, l.Route, l.Status, l.DurationMS, l.FirstByteMS,
		l.InputTokens, l.OutputTokens, l.CacheReadTokens, l.CacheWriteTokens, l.ChargePicoUSD, l.Estimated, l.ErrorCode, l.ErrorMessage)
	if err != nil {
		return err
	}
	if len(l.AuditIDs) > 0 {
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		for _, auditID := range l.AuditIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE audit_record_details SET usage_log_id = ? WHERE audit_id = ?`, id, auditID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func usageWhere(f usageFilter) (string, []any) {
	parts := []string{"at_ms >= ?"}
	args := []any{f.Since.UnixMilli()}
	if f.ID > 0 {
		parts = append(parts, "id = ?")
		args = append(args, f.ID)
	}
	if !f.Until.IsZero() {
		parts = append(parts, "at_ms < ?")
		args = append(args, f.Until.UnixMilli())
	}
	if f.UserID != "" {
		parts = append(parts, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.Model != "" {
		parts = append(parts, "model = ?")
		args = append(args, f.Model)
	}
	if f.KeyID != "" {
		parts = append(parts, "key_id = ?")
		args = append(args, f.KeyID)
	}
	if f.AccountID != "" {
		parts = append(parts, "account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.Route != "" {
		parts = append(parts, "route = ?")
		args = append(args, f.Route)
	}
	if f.Status == "success" {
		parts = append(parts, "status BETWEEN 200 AND 299")
	}
	if f.Status == "error" {
		parts = append(parts, "status NOT BETWEEN 200 AND 299")
	}
	return strings.Join(parts, " AND "), args
}
func (s *usageLogStore) list(ctx context.Context, f usageFilter) ([]usageLog, bool, error) {
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 50
	}
	if f.Page < 1 {
		f.Page = 1
	}
	where, args := usageWhere(f)
	args = append(args, f.Limit+1, (f.Page-1)*f.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+usageLogSelectColumns+` FROM usage_logs WHERE `+where+` ORDER BY at_ms DESC,id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]usageLog, 0, f.Limit)
	for rows.Next() {
		l, err := scanUsageLog(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > f.Limit
	if more {
		out = out[:f.Limit]
	}
	return out, more, nil
}

func (s *usageLogStore) logsByIDs(ctx context.Context, ids []int64) ([]usageLog, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i], args[i] = "?", id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+usageLogSelectColumns+` FROM usage_logs WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]usageLog, 0, len(ids))
	for rows.Next() {
		log, err := scanUsageLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, log)
	}
	return out, rows.Err()
}

func scanUsageLog(rows *sql.Rows) (usageLog, error) {
	var l usageLog
	var at int64
	var estimated int
	var firstByte sql.NullInt64
	if err := rows.Scan(&l.ID, &at, &l.UserID, &l.Username, &l.KeyID, &l.PlanID, &l.AccountID, &l.AccountPrefix, &l.Model, &l.Route, &l.Status, &l.DurationMS, &firstByte, &l.InputTokens, &l.OutputTokens, &l.CacheReadTokens, &l.CacheWriteTokens, &l.ChargePicoUSD, &estimated, &l.ErrorCode, &l.ErrorMessage); err != nil {
		return usageLog{}, err
	}
	if firstByte.Valid {
		value := int(firstByte.Int64)
		l.FirstByteMS = &value
	}
	l.At = time.UnixMilli(at)
	l.Estimated = estimated != 0
	return l, nil
}
func (s *usageLogStore) summary(ctx context.Context, f usageFilter) (usageSummary, error) {
	where, args := usageWhere(f)
	var v usageSummary
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(charge_pico_usd),0) FROM usage_logs WHERE `+where, args...).Scan(&v.Requests, &v.Successful, &v.InputTokens, &v.OutputTokens, &v.ChargePicoUSD)
	return v, err
}

type logResponseWriter struct {
	http.ResponseWriter
	status      int
	started     time.Time
	firstByteMS *int
	errorBody   []byte
}

func (w *logResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *logResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.errorBody) < 16<<10 {
		remaining := 16<<10 - len(w.errorBody)
		sample := p
		if len(sample) > remaining {
			sample = sample[:remaining]
		}
		w.errorBody = append(w.errorBody, sample...)
	}
	firstWrite := time.Now()
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && w.firstByteMS == nil {
		elapsed := int(firstWrite.Sub(w.started).Milliseconds())
		if elapsed < 0 {
			elapsed = 0
		}
		w.firstByteMS = &elapsed
	}
	return n, err
}
func (w *logResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (w *logResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *logResponseWriter) errorDetails() (string, string) {
	if w.status < 400 {
		return "", ""
	}
	code := ""
	message := ""
	raw := bytes.TrimSpace(w.errorBody)
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) == nil {
		code = errorJSONText(root["code"])
		message = errorJSONText(root["message"])
		if message == "" {
			message = errorJSONText(root["error_description"])
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(root["error"], &nested) == nil {
			if value := errorJSONText(nested["code"]); value != "" {
				code = value
			}
			if code == "" {
				code = errorJSONText(nested["type"])
			}
			if value := errorJSONText(nested["message"]); value != "" {
				message = value
			}
		} else if value := errorJSONText(root["error"]); value != "" {
			if message == "" {
				message = value
			}
		}
	} else {
		if !bytes.HasPrefix(bytes.ToLower(raw), []byte("<!doctype html")) && !bytes.HasPrefix(bytes.ToLower(raw), []byte("<html")) {
			message = string(raw)
		}
	}
	if code == "" {
		code = "HTTP " + strconv.Itoa(w.status)
	}
	if message == "" {
		message = http.StatusText(w.status)
	}
	return shortErrorText(code, 120), shortErrorText(message, 800)
}

func errorJSONText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

func shortErrorText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(strings.ToValidUTF8(value, "")), " ")
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes]) + "…"
	}
	return value
}

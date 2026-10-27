package companion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type auditRecord struct {
	ID                      int64
	At                      time.Time
	UserID, Username, KeyID string
	PlanID, Model           string
	Route, Category         string
	InputText               string
	FirstInSession          bool
	Hit, Fallback           bool
	ScorePercent            float64
	ThresholdPercent        int
	DurationMS              int
	UsageLogID              int64
	EndpointName            string
	AttemptCount            int
}

func (r auditRecord) AtText() string { return r.At.In(beijing).Format("2006-01-02 15:04:05") }
func (r auditRecord) ResultText() string {
	if r.Hit {
		return "命中"
	}
	return "未命中"
}
func (r auditRecord) SourceText() string {
	if r.Category == "cyber_policy" || r.Category == "cyber_abuse" {
		return "OpenAI 上游"
	}
	if r.Fallback {
		return "关键词回退"
	}
	if r.EndpointName != "" {
		return "SystemOne · " + r.EndpointName
	}
	return "SystemOne"
}
func (r auditRecord) LogURL() string {
	if r.UsageLogID == 0 {
		return ""
	}
	return fmt.Sprintf("/logs?scope=all&log_id=%d", r.UsageLogID)
}
func (r auditRecord) ScoreText() string {
	if r.Category == "cyber_policy" || r.Category == "cyber_abuse" {
		return "上游标记"
	}
	if r.Fallback {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", r.ScorePercent)
}
func (r auditRecord) ReasonText() string {
	if r.Category == "cyber_policy" || r.Category == "cyber_abuse" {
		return "上游标记"
	}
	if r.FirstInSession {
		return "会话首条"
	}
	return "随机抽查"
}

type auditSummary struct {
	Checked, Hits, Fallbacks int64
}

func (s *usageLogStore) insertAudit(ctx context.Context, r auditRecord) (int64, error) {
	if r.UserID == "" || r.KeyID == "" {
		return 0, errors.New("invalid content audit record")
	}
	if r.Username == "" {
		r.Username = r.UserID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO content_audit_records
	(at_ms,user_id,username,key_id,plan_id,model,route,first_in_session,hit,fallback,category,score_percent,threshold_percent,input_text,duration_ms)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.At.UnixMilli(), r.UserID, r.Username, r.KeyID, r.PlanID, r.Model, r.Route,
		r.FirstInSession, r.Hit, r.Fallback, r.Category, r.ScorePercent, r.ThresholdPercent, r.InputText, r.DurationMS)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_record_details (audit_id,endpoint_name,attempts) VALUES (?,?,?)`, id, r.EndpointName, r.AttemptCount); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *usageLogStore) auditStats(ctx context.Context) (auditSummary, error) {
	var v auditSummary
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(hit),0),COALESCE(SUM(fallback),0) FROM content_audit_records`).Scan(&v.Checked, &v.Hits, &v.Fallbacks)
	return v, err
}

func (s *usageLogStore) clearAuditRecords(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_record_details`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_audit_records`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *usageLogStore) auditRecords(ctx context.Context, userID, result string, page int) ([]auditRecord, bool, error) {
	if page < 1 {
		page = 1
	}
	where := []string{"1=1"}
	var args []any
	if userID != "" {
		where = append(where, "user_id = ?")
		args = append(args, userID)
	}
	switch result {
	case "hit":
		where = append(where, "hit = 1")
	case "clear":
		where = append(where, "hit = 0")
	case "fallback":
		where = append(where, "fallback = 1")
	}
	args = append(args, 51, (page-1)*50)
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.at_ms,a.user_id,a.username,a.key_id,a.plan_id,a.model,a.route,a.first_in_session,a.hit,a.fallback,
 a.category,a.score_percent,a.threshold_percent,a.input_text,a.duration_ms,COALESCE(d.usage_log_id,0),COALESCE(d.endpoint_name,''),COALESCE(d.attempts,0)
 FROM content_audit_records a LEFT JOIN audit_record_details d ON d.audit_id = a.id WHERE `+strings.Join(where, " AND ")+` ORDER BY a.at_ms DESC,a.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var resultRows []auditRecord
	for rows.Next() {
		var record auditRecord
		var at int64
		if err := rows.Scan(&record.ID, &at, &record.UserID, &record.Username, &record.KeyID, &record.PlanID, &record.Model, &record.Route,
			&record.FirstInSession, &record.Hit, &record.Fallback, &record.Category, &record.ScorePercent,
			&record.ThresholdPercent, &record.InputText, &record.DurationMS, &record.UsageLogID, &record.EndpointName, &record.AttemptCount); err != nil {
			return nil, false, err
		}
		record.At = time.UnixMilli(at)
		resultRows = append(resultRows, record)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasNext := len(resultRows) > 50
	if hasNext {
		resultRows = resultRows[:50]
	}
	return resultRows, hasNext, nil
}

package report

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/claw-works/pincer/internal/store"
	"github.com/google/uuid"
)

// DailyReport stores a project's daily summary.
type DailyReport struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Date      string    `json:"date"` // YYYY-MM-DD (Asia/Shanghai)
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

// PGStore handles daily report persistence.
type PGStore struct {
	db *store.DB
}

func NewPGStore(db *store.DB) *PGStore {
	return &PGStore{db: db}
}

func (s *PGStore) Save(ctx context.Context, projectID, date, summary string) (*DailyReport, error) {
	r := &DailyReport{
		ID:        uuid.New().String(),
		ProjectID: projectID,
		Date:      date,
		Summary:   summary,
		CreatedAt: time.Now(),
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO daily_reports (id, project_id, date, summary, created_at)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (project_id, date) DO UPDATE SET summary=EXCLUDED.summary, created_at=EXCLUDED.created_at`,
		r.ID, r.ProjectID, r.Date, r.Summary, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("save report: %w", err)
	}
	return r, nil
}

func (s *PGStore) ListByProject(ctx context.Context, projectID string, limit int) ([]*DailyReport, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 365 {
		limit = 365
	}
	rows, err := s.db.PG.Query(ctx,
		`SELECT id, project_id, date, summary, created_at
		 FROM daily_reports WHERE project_id=$1
		 ORDER BY date DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []*DailyReport
	for rows.Next() {
		r := &DailyReport{}
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Date, &r.Summary, &r.CreatedAt); err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	return reports, rows.Err()
}

func (s *PGStore) GetLatest(ctx context.Context, projectID string) (*DailyReport, error) {
	r := &DailyReport{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, project_id, date, summary, created_at
		 FROM daily_reports WHERE project_id=$1
		 ORDER BY date DESC LIMIT 1`, projectID,
	).Scan(&r.ID, &r.ProjectID, &r.Date, &r.Summary, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ScheduleDaily blocks and fires the daily report at 15:30 UTC (23:30 CST) every day.
func ScheduleDaily(ctx context.Context, fire func(ctx context.Context)) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), 15, 30, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		wait := next.Sub(now)
		log.Printf("daily-report: next run in %s (at %s UTC)", wait.Round(time.Second), next.Format("2006-01-02 15:04"))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			fire(ctx)
		}
	}
}

// FormatReport generates a markdown summary for a project.
func FormatReport(projectName, date string, statusCounts map[string]int, totalTasks int, prevSummary string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 **%s 日报**（%s 北京时间）\n\n", projectName, date))
	sb.WriteString(fmt.Sprintf("任务概况：共 %d 个\n", totalTasks))
	if v := statusCounts["done"]; v > 0 {
		sb.WriteString(fmt.Sprintf("  ✅ 已完成：%d\n", v))
	}
	if v := statusCounts["running"]; v > 0 {
		sb.WriteString(fmt.Sprintf("  🔄 进行中：%d\n", v))
	}
	if v := statusCounts["assigned"]; v > 0 {
		sb.WriteString(fmt.Sprintf("  📌 已分配：%d\n", v))
	}
	if v := statusCounts["pending"]; v > 0 {
		sb.WriteString(fmt.Sprintf("  ⏳ 待处理：%d\n", v))
	}
	if v := statusCounts["failed"]; v > 0 {
		sb.WriteString(fmt.Sprintf("  ❌ 失败：%d\n", v))
	}
	if prevSummary != "" {
		sb.WriteString("\n**昨日总结：** " + prevSummary + "\n")
	}
	return sb.String()
}

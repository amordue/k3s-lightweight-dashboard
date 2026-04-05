package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"k3s-dashboard/internal/model"
)

var ErrNoData = errors.New("no metrics data available")

type Store struct {
	db        *sql.DB
	retention time.Duration
}

func Open(path string, retention time.Duration, failIfDataDirExists bool) (*Store, error) {
	dataDir := filepath.Dir(path)
	if _, err := os.Stat(dataDir); err == nil {
		if failIfDataDirExists {
			return nil, fmt.Errorf("data directory already exists: %s", dataDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat data directory %s: %w", dataDir, err)
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	statements := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		`CREATE TABLE IF NOT EXISTS pod_metrics (
			scraped_at TEXT NOT NULL,
			namespace TEXT NOT NULL,
			workload_kind TEXT NOT NULL,
			workload_name TEXT NOT NULL,
			pod_name TEXT NOT NULL,
			node_name TEXT NOT NULL,
			cpu_milli INTEGER NOT NULL,
			memory_bytes INTEGER NOT NULL,
			PRIMARY KEY (scraped_at, namespace, pod_name)
		);`,
		"CREATE INDEX IF NOT EXISTS idx_pod_metrics_scraped_at ON pod_metrics(scraped_at);",
		"CREATE INDEX IF NOT EXISTS idx_pod_metrics_workload ON pod_metrics(namespace, workload_kind, workload_name, scraped_at);",
	}

	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return nil, err
		}
	}

	return &Store{db: db, retention: retention}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Retention() time.Duration {
	return s.retention
}

func (s *Store) InsertSnapshot(ctx context.Context, scrapedAt time.Time, samples []model.PodSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	statement, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO pod_metrics (
			scraped_at,
			namespace,
			workload_kind,
			workload_name,
			pod_name,
			node_name,
			cpu_milli,
			memory_bytes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer statement.Close()

	formattedTime := scrapedAt.UTC().Format(time.RFC3339)
	for _, sample := range samples {
		if _, err := statement.ExecContext(
			ctx,
			formattedTime,
			sample.Namespace,
			sample.WorkloadKind,
			sample.WorkloadName,
			sample.PodName,
			sample.NodeName,
			sample.CPUMilli,
			sample.MemoryBytes,
		); err != nil {
			statement.Close()
			tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) PruneBefore(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM pod_metrics WHERE scraped_at < ?", cutoff.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) LatestSummary(ctx context.Context) (model.ClusterSummary, error) {
	latestScrape, err := s.latestScrape(ctx)
	if err != nil {
		return model.ClusterSummary{}, err
	}

	summary := model.ClusterSummary{LatestScrape: latestScrape}

	if err := s.db.QueryRowContext(
		ctx,
		"SELECT COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0) FROM pod_metrics WHERE scraped_at = ?",
		latestScrape.Format(time.RFC3339),
	).Scan(&summary.Totals.CPUMilli, &summary.Totals.MemoryBytes); err != nil {
		return model.ClusterSummary{}, err
	}

	namespaceRows, err := s.db.QueryContext(ctx, `
		SELECT namespace,
			COALESCE(SUM(cpu_milli), 0),
			COALESCE(SUM(memory_bytes), 0),
			COUNT(DISTINCT workload_kind || '/' || workload_name),
			COUNT(*)
		FROM pod_metrics
		WHERE scraped_at = ?
		GROUP BY namespace
		ORDER BY SUM(cpu_milli) DESC, namespace ASC
	`, latestScrape.Format(time.RFC3339))
	if err != nil {
		return model.ClusterSummary{}, err
	}
	defer namespaceRows.Close()

	for namespaceRows.Next() {
		var item model.NamespaceSummary
		if err := namespaceRows.Scan(
			&item.Namespace,
			&item.CPUMilli,
			&item.MemoryBytes,
			&item.WorkloadCount,
			&item.PodCount,
		); err != nil {
			return model.ClusterSummary{}, err
		}
		summary.Namespaces = append(summary.Namespaces, item)
	}

	workloadRows, err := s.db.QueryContext(ctx, `
		SELECT namespace, workload_kind, workload_name,
			COALESCE(SUM(cpu_milli), 0),
			COALESCE(SUM(memory_bytes), 0),
			COUNT(*)
		FROM pod_metrics
		WHERE scraped_at = ?
		GROUP BY namespace, workload_kind, workload_name
		ORDER BY SUM(cpu_milli) DESC, namespace ASC, workload_name ASC
	`, latestScrape.Format(time.RFC3339))
	if err != nil {
		return model.ClusterSummary{}, err
	}
	defer workloadRows.Close()

	for workloadRows.Next() {
		var item model.WorkloadSummary
		if err := workloadRows.Scan(
			&item.Namespace,
			&item.WorkloadKind,
			&item.WorkloadName,
			&item.CPUMilli,
			&item.MemoryBytes,
			&item.PodCount,
		); err != nil {
			return model.ClusterSummary{}, err
		}
		summary.Workloads = append(summary.Workloads, item)
	}

	podRows, err := s.db.QueryContext(ctx, `
		SELECT namespace, workload_kind, workload_name, pod_name, node_name, cpu_milli, memory_bytes
		FROM pod_metrics
		WHERE scraped_at = ?
		ORDER BY cpu_milli DESC, namespace ASC, pod_name ASC
	`, latestScrape.Format(time.RFC3339))
	if err != nil {
		return model.ClusterSummary{}, err
	}
	defer podRows.Close()

	for podRows.Next() {
		var item model.PodSummary
		if err := podRows.Scan(
			&item.Namespace,
			&item.WorkloadKind,
			&item.WorkloadName,
			&item.PodName,
			&item.NodeName,
			&item.CPUMilli,
			&item.MemoryBytes,
		); err != nil {
			return model.ClusterSummary{}, err
		}
		summary.Pods = append(summary.Pods, item)
	}

	return summary, nil
}

func (s *Store) ListWorkloads(ctx context.Context) ([]model.WorkloadSummary, error) {
	latestScrape, err := s.latestScrape(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT namespace, workload_kind, workload_name,
			COALESCE(SUM(cpu_milli), 0),
			COALESCE(SUM(memory_bytes), 0),
			COUNT(*)
		FROM pod_metrics
		WHERE scraped_at = ?
		GROUP BY namespace, workload_kind, workload_name
		ORDER BY namespace ASC, workload_kind ASC, workload_name ASC
	`, latestScrape.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	workloads := make([]model.WorkloadSummary, 0)
	for rows.Next() {
		var item model.WorkloadSummary
		if err := rows.Scan(
			&item.Namespace,
			&item.WorkloadKind,
			&item.WorkloadName,
			&item.CPUMilli,
			&item.MemoryBytes,
			&item.PodCount,
		); err != nil {
			return nil, err
		}
		workloads = append(workloads, item)
	}

	return workloads, nil
}

func (s *Store) ClusterHistory(ctx context.Context, window time.Duration) (model.ClusterHistoryResponse, error) {
	latestScrape, err := s.latestScrape(ctx)
	if err != nil {
		return model.ClusterHistoryResponse{}, err
	}

	history, err := s.historyQuery(ctx, `
		SELECT scraped_at, COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0)
		FROM pod_metrics
		WHERE scraped_at >= ?
		GROUP BY scraped_at
		ORDER BY scraped_at ASC
	`, latestScrape.Add(-window).Format(time.RFC3339))
	if err != nil {
		return model.ClusterHistoryResponse{}, err
	}

	return model.ClusterHistoryResponse{
		LatestScrape: latestScrape,
		Window:       window.String(),
		History:      history,
	}, nil
}

func (s *Store) NamespaceDetail(ctx context.Context, namespace string, window time.Duration) (model.NamespaceDetail, error) {
	latestScrape, err := s.latestScrape(ctx)
	if err != nil {
		return model.NamespaceDetail{}, err
	}

	detail := model.NamespaceDetail{LatestScrape: latestScrape, Namespace: namespace}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0)
		FROM pod_metrics
		WHERE scraped_at = ? AND namespace = ?
	`, latestScrape.Format(time.RFC3339), namespace).Scan(&detail.Totals.CPUMilli, &detail.Totals.MemoryBytes); err != nil {
		return model.NamespaceDetail{}, err
	}

	workloads, err := s.db.QueryContext(ctx, `
		SELECT namespace, workload_kind, workload_name,
			COALESCE(SUM(cpu_milli), 0),
			COALESCE(SUM(memory_bytes), 0),
			COUNT(*)
		FROM pod_metrics
		WHERE scraped_at = ? AND namespace = ?
		GROUP BY namespace, workload_kind, workload_name
		ORDER BY SUM(cpu_milli) DESC, workload_name ASC
	`, latestScrape.Format(time.RFC3339), namespace)
	if err != nil {
		return model.NamespaceDetail{}, err
	}
	defer workloads.Close()

	for workloads.Next() {
		var item model.WorkloadSummary
		if err := workloads.Scan(
			&item.Namespace,
			&item.WorkloadKind,
			&item.WorkloadName,
			&item.CPUMilli,
			&item.MemoryBytes,
			&item.PodCount,
		); err != nil {
			return model.NamespaceDetail{}, err
		}
		detail.Workloads = append(detail.Workloads, item)
	}

	pods, err := s.db.QueryContext(ctx, `
		SELECT namespace, workload_kind, workload_name, pod_name, node_name, cpu_milli, memory_bytes
		FROM pod_metrics
		WHERE scraped_at = ? AND namespace = ?
		ORDER BY cpu_milli DESC, pod_name ASC
	`, latestScrape.Format(time.RFC3339), namespace)
	if err != nil {
		return model.NamespaceDetail{}, err
	}
	defer pods.Close()

	for pods.Next() {
		var item model.PodSummary
		if err := pods.Scan(
			&item.Namespace,
			&item.WorkloadKind,
			&item.WorkloadName,
			&item.PodName,
			&item.NodeName,
			&item.CPUMilli,
			&item.MemoryBytes,
		); err != nil {
			return model.NamespaceDetail{}, err
		}
		detail.Pods = append(detail.Pods, item)
	}

	history, err := s.historyQuery(ctx, `
		SELECT scraped_at, COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0)
		FROM pod_metrics
		WHERE namespace = ? AND scraped_at >= ?
		GROUP BY scraped_at
		ORDER BY scraped_at ASC
	`, namespace, latestScrape.Add(-window).Format(time.RFC3339))
	if err != nil {
		return model.NamespaceDetail{}, err
	}
	detail.History = history

	return detail, nil
}

func (s *Store) WorkloadDetail(ctx context.Context, namespace string, kind string, name string, window time.Duration) (model.WorkloadDetail, error) {
	latestScrape, err := s.latestScrape(ctx)
	if err != nil {
		return model.WorkloadDetail{}, err
	}

	detail := model.WorkloadDetail{
		LatestScrape: latestScrape,
		Namespace:    namespace,
		WorkloadKind: kind,
		WorkloadName: name,
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0)
		FROM pod_metrics
		WHERE scraped_at = ? AND namespace = ? AND workload_kind = ? AND workload_name = ?
	`, latestScrape.Format(time.RFC3339), namespace, kind, name).Scan(&detail.Totals.CPUMilli, &detail.Totals.MemoryBytes); err != nil {
		return model.WorkloadDetail{}, err
	}

	pods, err := s.db.QueryContext(ctx, `
		SELECT namespace, workload_kind, workload_name, pod_name, node_name, cpu_milli, memory_bytes
		FROM pod_metrics
		WHERE scraped_at = ? AND namespace = ? AND workload_kind = ? AND workload_name = ?
		ORDER BY cpu_milli DESC, pod_name ASC
	`, latestScrape.Format(time.RFC3339), namespace, kind, name)
	if err != nil {
		return model.WorkloadDetail{}, err
	}
	defer pods.Close()

	for pods.Next() {
		var item model.PodSummary
		if err := pods.Scan(
			&item.Namespace,
			&item.WorkloadKind,
			&item.WorkloadName,
			&item.PodName,
			&item.NodeName,
			&item.CPUMilli,
			&item.MemoryBytes,
		); err != nil {
			return model.WorkloadDetail{}, err
		}
		detail.Pods = append(detail.Pods, item)
	}

	history, err := s.historyQuery(ctx, `
		SELECT scraped_at, COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0)
		FROM pod_metrics
		WHERE namespace = ? AND workload_kind = ? AND workload_name = ? AND scraped_at >= ?
		GROUP BY scraped_at
		ORDER BY scraped_at ASC
	`, namespace, kind, name, latestScrape.Add(-window).Format(time.RFC3339))
	if err != nil {
		return model.WorkloadDetail{}, err
	}
	detail.History = history

	return detail, nil
}

func (s *Store) WorkloadHistory(ctx context.Context, selectors []model.WorkloadSelector, window time.Duration) (model.WorkloadHistoryResponse, error) {
	latestScrape, err := s.latestScrape(ctx)
	if err != nil {
		return model.WorkloadHistoryResponse{}, err
	}

	response := model.WorkloadHistoryResponse{
		LatestScrape: latestScrape,
		Window:       window.String(),
		Series:       make([]model.WorkloadHistorySeries, 0, len(selectors)),
	}

	for _, selector := range selectors {
		series := model.WorkloadHistorySeries{
			Namespace:    selector.Namespace,
			WorkloadKind: selector.WorkloadKind,
			WorkloadName: selector.WorkloadName,
		}

		if err := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0)
			FROM pod_metrics
			WHERE scraped_at = ? AND namespace = ? AND workload_kind = ? AND workload_name = ?
		`, latestScrape.Format(time.RFC3339), selector.Namespace, selector.WorkloadKind, selector.WorkloadName).Scan(
			&series.Totals.CPUMilli,
			&series.Totals.MemoryBytes,
		); err != nil {
			return model.WorkloadHistoryResponse{}, err
		}

		history, err := s.historyQuery(ctx, `
			SELECT scraped_at, COALESCE(SUM(cpu_milli), 0), COALESCE(SUM(memory_bytes), 0)
			FROM pod_metrics
			WHERE namespace = ? AND workload_kind = ? AND workload_name = ? AND scraped_at >= ?
			GROUP BY scraped_at
			ORDER BY scraped_at ASC
		`, selector.Namespace, selector.WorkloadKind, selector.WorkloadName, latestScrape.Add(-window).Format(time.RFC3339))
		if err != nil {
			return model.WorkloadHistoryResponse{}, err
		}

		series.History = history
		response.Series = append(response.Series, series)
	}

	return response, nil
}

func (s *Store) PodDetail(ctx context.Context, namespace string, name string, window time.Duration) (model.PodDetail, error) {
	latestScrape, err := s.latestScrape(ctx)
	if err != nil {
		return model.PodDetail{}, err
	}

	detail := model.PodDetail{LatestScrape: latestScrape, Namespace: namespace, PodName: name}
	if err := s.db.QueryRowContext(ctx, `
		SELECT workload_kind, workload_name, node_name, cpu_milli, memory_bytes
		FROM pod_metrics
		WHERE scraped_at = ? AND namespace = ? AND pod_name = ?
	`, latestScrape.Format(time.RFC3339), namespace, name).Scan(
		&detail.WorkloadKind,
		&detail.WorkloadName,
		&detail.NodeName,
		&detail.Totals.CPUMilli,
		&detail.Totals.MemoryBytes,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.PodDetail{}, ErrNoData
		}
		return model.PodDetail{}, err
	}

	history, err := s.historyQuery(ctx, `
		SELECT scraped_at, cpu_milli, memory_bytes
		FROM pod_metrics
		WHERE namespace = ? AND pod_name = ? AND scraped_at >= ?
		ORDER BY scraped_at ASC
	`, namespace, name, latestScrape.Add(-window).Format(time.RFC3339))
	if err != nil {
		return model.PodDetail{}, err
	}
	detail.History = history

	return detail, nil
}

func (s *Store) latestScrape(ctx context.Context) (time.Time, error) {
	var timestamp string
	if err := s.db.QueryRowContext(ctx, "SELECT scraped_at FROM pod_metrics ORDER BY scraped_at DESC LIMIT 1").Scan(&timestamp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrNoData
		}
		return time.Time{}, err
	}

	parsed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse latest scrape time: %w", err)
	}

	return parsed, nil
}

func (s *Store) historyQuery(ctx context.Context, query string, args ...any) ([]model.Point, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	history := make([]model.Point, 0)
	for rows.Next() {
		var timestamp string
		var point model.Point
		if err := rows.Scan(&timestamp, &point.CPUMilli, &point.MemoryBytes); err != nil {
			return nil, err
		}

		point.Timestamp, err = time.Parse(time.RFC3339, timestamp)
		if err != nil {
			return nil, err
		}

		history = append(history, point)
	}

	return history, nil
}
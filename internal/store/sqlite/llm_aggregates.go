package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// dayLayout is the UTC day key shared with the migration backfill.
const dayLayout = "2006-01-02"

type callDailyCounters struct {
	calls          int64
	errors         int64
	inputTokens    int64
	outputTokens   int64
	cacheRead      int64
	cacheWrite     int64
	latencyMS      int64
	latencySamples int64
}

// recordLLMCallAggregatesTx folds one audited call into the long-lived daily
// tables. The workflow's created day and project keep the Stats filter
// semantics identical to the llm_calls JOIN workflows query. A missing workflow
// is skipped: the join would never count the call either (#459).
func recordLLMCallAggregatesTx(ctx context.Context, tx *sql.Tx, call llm.CallRecord, counters callDailyCounters, errorKind string) error {
	var (
		projectID  int64
		createdRaw string
	)
	err := tx.QueryRowContext(ctx, `SELECT project_id, created_at FROM workflows WHERE id = ?`,
		call.Meta.WorkflowID).Scan(&projectID, &createdRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlite: aggregate workflow lookup: %w", err)
	}
	created, err := parseTime(createdRaw)
	if err != nil {
		return err
	}
	day := created.UTC().Format(dayLayout)

	existing, found, err := getDailyCountersTx(ctx, tx, day, projectID, call.Provider, call.Model)
	if err != nil {
		return err
	}
	merged := callDailyCounters{
		calls:          saturatingAddInt64(existing.calls, counters.calls),
		errors:         saturatingAddInt64(existing.errors, counters.errors),
		inputTokens:    saturatingAddInt64(existing.inputTokens, counters.inputTokens),
		outputTokens:   saturatingAddInt64(existing.outputTokens, counters.outputTokens),
		cacheRead:      saturatingAddInt64(existing.cacheRead, counters.cacheRead),
		cacheWrite:     saturatingAddInt64(existing.cacheWrite, counters.cacheWrite),
		latencyMS:      saturatingAddInt64(existing.latencyMS, counters.latencyMS),
		latencySamples: saturatingAddInt64(existing.latencySamples, counters.latencySamples),
	}
	if !found {
		// A missing row is all zeroes; the merge above is the first value.
		merged = counters
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO llm_call_daily
		(day, project_id, provider, model, calls, errors, input_tokens, output_tokens,
		 cache_read, cache_write, latency_ms_sum, latency_samples)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (day, project_id, provider, model) DO UPDATE SET
			calls = excluded.calls,
			errors = excluded.errors,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cache_read = excluded.cache_read,
			cache_write = excluded.cache_write,
			latency_ms_sum = excluded.latency_ms_sum,
			latency_samples = excluded.latency_samples`,
		day, projectID, call.Provider, call.Model, merged.calls, merged.errors,
		merged.inputTokens, merged.outputTokens, merged.cacheRead, merged.cacheWrite,
		merged.latencyMS, merged.latencySamples); err != nil {
		return fmt.Errorf("sqlite: aggregate llm call: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO llm_call_latency_daily
		(day, project_id, provider, model, latency_ms, samples)
		VALUES (?, ?, ?, ?, ?, 1)
		ON CONFLICT (day, project_id, provider, model, latency_ms) DO UPDATE SET
			samples = samples + 1`,
		day, projectID, call.Provider, call.Model, counters.latencyMS); err != nil {
		return fmt.Errorf("sqlite: aggregate llm latency: %w", err)
	}
	if errorKind == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO llm_call_error_daily
		(day, project_id, provider, model, error_kind, calls)
		VALUES (?, ?, ?, ?, ?, 1)
		ON CONFLICT (day, project_id, provider, model, error_kind) DO UPDATE SET
			calls = calls + 1`,
		day, projectID, call.Provider, call.Model, errorKind); err != nil {
		return fmt.Errorf("sqlite: aggregate llm error: %w", err)
	}
	return nil
}

func getDailyCountersTx(ctx context.Context, tx *sql.Tx, day string, projectID int64, provider, model string) (callDailyCounters, bool, error) {
	var counters callDailyCounters
	err := tx.QueryRowContext(ctx, `SELECT calls, errors, input_tokens, output_tokens,
		cache_read, cache_write, latency_ms_sum, latency_samples
		FROM llm_call_daily WHERE day = ? AND project_id = ? AND provider = ? AND model = ?`,
		day, projectID, provider, model).Scan(
		&counters.calls, &counters.errors, &counters.inputTokens, &counters.outputTokens,
		&counters.cacheRead, &counters.cacheWrite, &counters.latencyMS, &counters.latencySamples)
	if errors.Is(err, sql.ErrNoRows) {
		return counters, false, nil
	}
	if err != nil {
		return counters, false, fmt.Errorf("sqlite: read llm daily aggregate: %w", err)
	}
	return counters, true, nil
}

// latencyHistogram is an exact millisecond histogram. Its percentile matches
// percentile() with nearest-rank selection because every integer latency is one
// bucket (#459).
type latencyHistogram map[int64]int64

func (h latencyHistogram) add(value int64, samples int64) {
	if samples <= 0 {
		return
	}
	h[value] = saturatingAddInt64(h[value], samples)
}

func (h latencyHistogram) samples() int64 {
	var total int64
	for _, count := range h {
		total = saturatingAddInt64(total, count)
	}
	return total
}

// percentile returns the nearest-rank percentile. It returns nil for an empty
// histogram so callers keep a nil field instead of a fake zero.
func (h latencyHistogram) percentile(p int) *float64 {
	total := h.samples()
	if total <= 0 {
		return nil
	}
	rank := (int64(p)*total + 99) / 100
	if rank < 1 {
		rank = 1
	}
	values := make([]int64, 0, len(h))
	for value := range h {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	var seen int64
	for _, value := range values {
		seen = saturatingAddInt64(seen, h[value])
		if seen >= rank {
			result := float64(value)
			return &result
		}
	}
	result := float64(values[len(values)-1])
	return &result
}

// modelTelemetry accumulates one provider/model entry from detail or daily
// rows.
type modelTelemetry struct {
	calls        int64
	errors       int64
	inputTokens  int64
	outputTokens int64
	latencyMS    float64
	latencies    latencyHistogram
	errorsByKind map[string]int64
}

func (m *modelTelemetry) addErrorKind(kind string) {
	if kind == "" {
		return
	}
	if m.errorsByKind == nil {
		m.errorsByKind = make(map[string]int64)
	}
	m.errorsByKind[kind] = saturatingAddInt64(m.errorsByKind[kind], 1)
}

// modelStats builds the per-model view. Detail rows are the exact source while
// they exist; rows older than the retained detail window are served from the
// long-lived daily aggregates, and ModelDetailsPurged tells the caller that the
// window mixes both (#459).
func (s *Store) modelStats(ctx context.Context, filter store.StatsFilter, where string, args []any) ([]store.ModelStats, bool, error) {
	detailSince, err := s.llmCallDetailSince(ctx)
	if err != nil {
		return nil, false, err
	}
	groups := make(map[string]*modelTelemetry)
	order := make([]string, 0)
	entry := func(provider, model string) *modelTelemetry {
		key := provider + "\x00" + model
		telemetry, ok := groups[key]
		if !ok {
			telemetry = &modelTelemetry{latencies: make(latencyHistogram)}
			groups[key] = telemetry
			order = append(order, key)
		}
		return telemetry
	}

	detailLower := filter.From
	if !detailSince.IsZero() && (detailLower.IsZero() || detailLower.Before(detailSince)) {
		detailLower = detailSince
	}
	detailWhere := where
	detailArgs := append([]any(nil), args...)
	if !detailLower.IsZero() {
		if detailWhere == "" {
			detailWhere = "WHERE "
		} else {
			detailWhere += " AND "
		}
		detailWhere += "c.created_at >= ?"
		detailArgs = append(detailArgs, formatTime(detailLower.UTC()))
	}
	detailRows, err := s.db.QueryContext(ctx, `SELECT
		c.provider, c.model, c.error_kind, c.input_tokens, c.output_tokens, c.latency_ms
		FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id `+detailWhere+`
		ORDER BY c.provider, c.model`, detailArgs...)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: stats llm calls: %w", err)
	}
	for detailRows.Next() {
		var (
			provider  string
			model     string
			errorKind sql.NullString
			input     int64
			output    int64
			latencyMS int64
		)
		if err := detailRows.Scan(&provider, &model, &errorKind, &input, &output, &latencyMS); err != nil {
			detailRows.Close()
			return nil, false, fmt.Errorf("sqlite: scan stats llm call: %w", err)
		}
		telemetry := entry(provider, model)
		telemetry.calls = saturatingAddInt64(telemetry.calls, 1)
		if errorKind.String != "" {
			telemetry.errors = saturatingAddInt64(telemetry.errors, 1)
			telemetry.addErrorKind(errorKind.String)
		}
		telemetry.inputTokens = saturatingAddInt64(telemetry.inputTokens, input)
		telemetry.outputTokens = saturatingAddInt64(telemetry.outputTokens, output)
		telemetry.latencyMS += float64(latencyMS)
		telemetry.latencies.add(latencyMS, 1)
	}
	if err := detailRows.Err(); err != nil {
		detailRows.Close()
		return nil, false, fmt.Errorf("sqlite: iterate stats llm calls: %w", err)
	}
	detailRows.Close()

	conditions, aggregateArgs := aggregateStatsConditions(filter, detailSince)
	aggregateWhere := ""
	if len(conditions) > 0 {
		aggregateWhere = "WHERE " + strings.Join(conditions, " AND ")
	}
	usedAggregates := false
	dailyRows, err := s.db.QueryContext(ctx, `SELECT
		d.provider, d.model, d.calls, d.errors, d.input_tokens, d.output_tokens,
		d.latency_ms_sum, d.latency_samples
		FROM llm_call_daily d `+aggregateWhere+`
		ORDER BY d.provider, d.model`, aggregateArgs...)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: stats llm daily aggregates: %w", err)
	}
	for dailyRows.Next() {
		var (
			provider string
			model    string
			counters callDailyCounters
		)
		if err := dailyRows.Scan(&provider, &model, &counters.calls, &counters.errors,
			&counters.inputTokens, &counters.outputTokens, &counters.latencyMS,
			&counters.latencySamples); err != nil {
			dailyRows.Close()
			return nil, false, fmt.Errorf("sqlite: scan stats llm daily aggregate: %w", err)
		}
		if counters.calls == 0 {
			continue
		}
		usedAggregates = true
		telemetry := entry(provider, model)
		telemetry.calls = saturatingAddInt64(telemetry.calls, counters.calls)
		telemetry.errors = saturatingAddInt64(telemetry.errors, counters.errors)
		telemetry.inputTokens = saturatingAddInt64(telemetry.inputTokens, counters.inputTokens)
		telemetry.outputTokens = saturatingAddInt64(telemetry.outputTokens, counters.outputTokens)
		telemetry.latencyMS += float64(counters.latencyMS)
	}
	if err := dailyRows.Err(); err != nil {
		dailyRows.Close()
		return nil, false, fmt.Errorf("sqlite: iterate stats llm daily aggregates: %w", err)
	}
	dailyRows.Close()
	purged := usedAggregates

	if usedAggregates {
		histogramRows, err := s.db.QueryContext(ctx, `SELECT
			d.provider, d.model, d.latency_ms, d.samples
			FROM llm_call_latency_daily d `+aggregateWhere+`
			ORDER BY d.provider, d.model, d.latency_ms`, aggregateArgs...)
		if err != nil {
			return nil, false, fmt.Errorf("sqlite: stats llm latency aggregates: %w", err)
		}
		for histogramRows.Next() {
			var (
				provider  string
				model     string
				latencyMS int64
				samples   int64
			)
			if err := histogramRows.Scan(&provider, &model, &latencyMS, &samples); err != nil {
				histogramRows.Close()
				return nil, false, fmt.Errorf("sqlite: scan stats llm latency aggregate: %w", err)
			}
			if _, ok := groups[provider+"\x00"+model]; !ok {
				continue
			}
			entry(provider, model).latencies.add(latencyMS, samples)
		}
		if err := histogramRows.Err(); err != nil {
			histogramRows.Close()
			return nil, false, fmt.Errorf("sqlite: iterate stats llm latency aggregates: %w", err)
		}
		histogramRows.Close()

		errorRows, err := s.db.QueryContext(ctx, `SELECT
			d.provider, d.model, d.error_kind, d.calls
			FROM llm_call_error_daily d `+aggregateWhere+`
			ORDER BY d.provider, d.model, d.error_kind`, aggregateArgs...)
		if err != nil {
			return nil, false, fmt.Errorf("sqlite: stats llm error aggregates: %w", err)
		}
		for errorRows.Next() {
			var (
				provider  string
				model     string
				errorKind string
				calls     int64
			)
			if err := errorRows.Scan(&provider, &model, &errorKind, &calls); err != nil {
				errorRows.Close()
				return nil, false, fmt.Errorf("sqlite: scan stats llm error aggregate: %w", err)
			}
			telemetry, ok := groups[provider+"\x00"+model]
			if !ok {
				continue
			}
			if telemetry.errorsByKind == nil {
				telemetry.errorsByKind = make(map[string]int64)
			}
			telemetry.errorsByKind[errorKind] = saturatingAddInt64(telemetry.errorsByKind[errorKind], calls)
		}
		if err := errorRows.Err(); err != nil {
			errorRows.Close()
			return nil, false, fmt.Errorf("sqlite: iterate stats llm error aggregates: %w", err)
		}
		errorRows.Close()
	}

	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	models := make([]store.ModelStats, 0, len(order))
	for _, key := range order {
		telemetry := groups[key]
		provider, model, _ := strings.Cut(key, "\x00")
		entry := store.ModelStats{
			Provider:     provider,
			Model:        model,
			Calls:        telemetry.calls,
			Errors:       telemetry.errors,
			InputTokens:  telemetry.inputTokens,
			OutputTokens: telemetry.outputTokens,
			ErrorsByKind: telemetry.errorsByKind,
		}
		if entry.Calls > 0 {
			rate := float64(entry.Errors) / float64(entry.Calls)
			entry.ErrorRate = &rate
			avg := telemetry.latencyMS / float64(entry.Calls)
			entry.AvgLatencyMS = &avg
		}
		entry.P50LatencyMS = telemetry.latencies.percentile(50)
		models = append(models, entry)
	}
	return models, purged, nil
}

// llmCallDetailSince returns the oldest remaining detail row, or zero when the
// audit table is empty. It delimits the range that the daily aggregates must
// cover without double counting (#459).
func (s *Store) llmCallDetailSince(ctx context.Context) (time.Time, error) {
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(created_at) FROM llm_calls`).Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("sqlite: llm detail range: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return time.Time{}, nil
	}
	return parseTime(raw.String)
}

// aggregateStatsConditions bounds the daily rows to the part of the window
// that no longer has detail rows. When details are gone entirely the whole
// window is served from aggregates at day granularity.
func aggregateStatsConditions(filter store.StatsFilter, detailSince time.Time) ([]string, []any) {
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if !filter.From.IsZero() {
		conditions = append(conditions, "d.day >= ?")
		args = append(args, filter.From.UTC().Format(dayLayout))
	}
	end := ""
	if !filter.To.IsZero() {
		end = aggregateEndDay(filter.To)
	}
	if !detailSince.IsZero() {
		detailDay := detailSince.UTC().Format(dayLayout)
		if end == "" || detailDay < end {
			end = detailDay
		}
	}
	if end != "" {
		conditions = append(conditions, "d.day < ?")
		args = append(args, end)
	}
	if filter.ProjectID != nil {
		conditions = append(conditions, "d.project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if clause, accessArgs := projectAccessClause("d.project_id", filter.AccessUserID); clause != "" {
		conditions = append(conditions, clause)
		args = append(args, accessArgs...)
	}
	return conditions, args
}

// aggregateEndDay returns the exclusive day bound for an exclusive time bound.
func aggregateEndDay(to time.Time) string {
	utc := to.UTC()
	day := utc.Format(dayLayout)
	if utc.Equal(time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)) {
		return day
	}
	return utc.Add(24 * time.Hour).Format(dayLayout)
}

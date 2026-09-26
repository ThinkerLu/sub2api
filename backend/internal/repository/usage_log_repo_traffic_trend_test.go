package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestUsageTrendIncludesTrafficForEveryQueryPath(t *testing.T) {
	for _, tc := range []struct {
		name        string
		granularity string
		userID      int64
		table       string
	}{
		{name: "hourly aggregate", granularity: "hour", table: "usage_dashboard_hourly"},
		{name: "daily aggregate", granularity: "day", table: "usage_dashboard_daily"},
		{name: "filtered raw logs", granularity: "hour", userID: 17, table: "usage_logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var query string
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &query}))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			repo := newUsageLogRepositoryWithSQL(nil, db)
			rows := sqlmock.NewRows([]string{"date", "requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost", "actual_cost", "ingress_body_bytes", "upstream_request_body_bytes", "upstream_response_body_bytes", "egress_body_bytes"}).
				AddRow("2026-09-25", 1, 2, 3, 4, 5, 14, 0.1, 0.05, 1024, 2048, 4096, 8192)
			mock.ExpectQuery("traffic trend").WillReturnRows(rows)
			start := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
			trend, err := repo.GetUsageTrendWithUsageFilters(context.Background(), start, start.Add(24*time.Hour), tc.granularity, usagestats.UsageLogFilters{UserID: tc.userID})
			require.NoError(t, err)
			require.Len(t, trend, 1)
			require.Equal(t, int64(1024), trend[0].IngressBodyBytes)
			require.Equal(t, int64(2048), trend[0].UpstreamRequestBodyBytes)
			require.Equal(t, int64(4096), trend[0].UpstreamResponseBodyBytes)
			require.Equal(t, int64(8192), trend[0].EgressBodyBytes)
			require.Equal(t, int64(14), trend[0].TotalTokens)
			require.Contains(t, query, "FROM "+tc.table)
			for _, field := range []string{"ingress_body_bytes", "upstream_request_body_bytes", "upstream_response_body_bytes", "egress_body_bytes"} {
				require.True(t, strings.Contains(query, field), "missing %s in %s", field, query)
			}
			if tc.userID != 0 {
				require.Contains(t, query, "user_id = $3")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUserUsageTrendIncludesTraffic(t *testing.T) {
	var query string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &query}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rows := sqlmock.NewRows([]string{"date", "requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost", "actual_cost", "ingress_body_bytes", "upstream_request_body_bytes", "upstream_response_body_bytes", "egress_body_bytes"}).
		AddRow("2026-09-25", 1, 2, 3, 4, 5, 14, 0.1, 0.05, 100, 200, 300, 400)
	mock.ExpectQuery("user traffic trend").WillReturnRows(rows)
	start := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	trend, err := newUsageLogRepositoryWithSQL(nil, db).GetUserUsageTrendByUserID(context.Background(), 17, start, start.Add(24*time.Hour), "day")
	require.NoError(t, err)
	require.Len(t, trend, 1)
	require.Equal(t, int64(100), trend[0].IngressBodyBytes)
	require.Equal(t, int64(400), trend[0].EgressBodyBytes)
	require.Contains(t, query, "WHERE user_id = $1")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageIngressTransportTrendBypassesUnfilteredAggregates(t *testing.T) {
	var query string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &query}))
	require.NoError(t, err)
	defer db.Close()
	rows := sqlmock.NewRows([]string{"date", "requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost", "actual_cost", "ingress_body_bytes", "upstream_request_body_bytes", "upstream_response_body_bytes", "egress_body_bytes"})
	mock.ExpectQuery("transport trend").WillReturnRows(rows)
	start := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	ingress := int16(1)
	_, err = newUsageLogRepositoryWithSQL(nil, db).GetUsageTrendWithUsageFilters(context.Background(), start, start.Add(24*time.Hour), "day", usagestats.UsageLogFilters{IngressTransport: &ingress})
	require.NoError(t, err)
	require.Contains(t, query, "FROM usage_logs")
	require.Contains(t, query, "ingress_transport = $3")
	require.NotContains(t, query, "usage_dashboard_daily")
	require.NoError(t, mock.ExpectationsWereMet())
}

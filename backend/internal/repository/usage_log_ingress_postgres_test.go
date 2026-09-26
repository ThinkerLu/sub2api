package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestUsageIngressTransportPostgresRoundTrip(t *testing.T) {
	dockerContext := os.Getenv("SUB2API_POSTGRES_PROBE_CONTEXT")
	if dockerContext == "" {
		t.Skip("set SUB2API_POSTGRES_PROBE_CONTEXT for an isolated PostgreSQL container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	docker := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", append([]string{"--context", dockerContext}, args...)...).CombinedOutput()
	}
	output, err := docker("run", "--rm", "-d", "--pull", "never", "--memory", "256m", "--tmpfs", "/var/lib/postgresql", "-e", "POSTGRES_PASSWORD=local-probe-only", "-p", "127.0.0.1::5432", "postgres:18-alpine")
	require.NoError(t, err, "%s", output)
	cid := strings.TrimSpace(string(output))
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "docker", "--context", dockerContext, "rm", "-f", cid).Run()
	}()
	output, err = docker("port", cid, "5432/tcp")
	require.NoError(t, err)
	address := strings.TrimSpace(string(output))
	db, err := sql.Open("postgres", "postgresql://postgres:local-probe-only@"+address+"/postgres?sslmode=disable")
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	require.Eventually(t, func() bool { return db.PingContext(ctx) == nil }, 20*time.Second, 100*time.Millisecond)
	columns := strings.Split(usageLogSelectColumns, ", ")[1:]
	var definitions []string
	for i, column := range columns {
		if column != "ingress_transport" {
			definitions = append(definitions, column+" "+usageLogInsertArgTypes[i])
		}
	}
	_, err = db.ExecContext(ctx, "CREATE TABLE usage_logs (id BIGSERIAL PRIMARY KEY, "+strings.Join(definitions, ", ")+", UNIQUE(request_id, api_key_id))")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO usage_logs (request_id,api_key_id) VALUES ('historical',1)")
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("242_usage_log_ingress_transport.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = db.ExecContext(ctx, string(migration))
		require.NoError(t, err)
	}
	var historical int16
	require.NoError(t, db.QueryRowContext(ctx, "SELECT ingress_transport FROM usage_logs WHERE request_id='historical'").Scan(&historical))
	require.Zero(t, historical)
	log := &service.UsageLog{UserID: 1, APIKeyID: 2, AccountID: 3, RequestID: "http-ws", Model: "gpt-5.1", RequestType: service.RequestTypeWSV2, IngressTransport: 1, UpstreamTransport: 2, CreatedAt: time.Now()}
	prepared := prepareUsageLogInsert(log)
	var placeholders []string
	for i := range columns {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
	}
	_, err = db.ExecContext(ctx, "INSERT INTO usage_logs ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(placeholders, ",")+")", prepared.args...)
	require.NoError(t, err)
	read, err := scanUsageLog(db.QueryRowContext(ctx, "SELECT "+usageLogSelectColumns+" FROM usage_logs WHERE request_id='http-ws'"))
	require.NoError(t, err)
	require.EqualValues(t, 1, read.IngressTransport)
	require.EqualValues(t, 2, read.UpstreamTransport)
	require.Equal(t, service.RequestTypeWSV2, read.RequestType)
	log.RequestID = "ws-http"
	log.IngressTransport = 2
	log.UpstreamTransport = 1
	prepared = prepareUsageLogInsert(log)
	key := usageLogBatchKey(log.RequestID, log.APIKeyID)
	query, args := buildUsageLogBatchInsertQuery([]string{key}, map[string]usageLogInsertPrepared{key: prepared})
	rows, err := db.QueryContext(ctx, query, args...)
	require.NoError(t, err)
	for rows.Next() {
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	read, err = scanUsageLog(db.QueryRowContext(ctx, "SELECT "+usageLogSelectColumns+" FROM usage_logs WHERE request_id='ws-http'"))
	require.NoError(t, err)
	require.EqualValues(t, 2, read.IngressTransport)
	// Simulate the previous binary's explicit column list after migration.
	log.RequestID = "old-binary"
	prepared = prepareUsageLogInsert(log)
	var oldColumns, oldPlaceholders []string
	var oldArgs []any
	for i, column := range columns {
		if column != "ingress_transport" {
			oldColumns = append(oldColumns, column)
			oldArgs = append(oldArgs, prepared.args[i])
			oldPlaceholders = append(oldPlaceholders, fmt.Sprintf("$%d", len(oldArgs)))
		}
	}
	_, err = db.ExecContext(ctx, "INSERT INTO usage_logs ("+strings.Join(oldColumns, ",")+") VALUES ("+strings.Join(oldPlaceholders, ",")+")", oldArgs...)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT ingress_transport FROM usage_logs WHERE request_id='old-binary'").Scan(&historical))
	require.Zero(t, historical)
}

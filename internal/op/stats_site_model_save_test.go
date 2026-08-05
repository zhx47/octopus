package op

import (
	"context"
	"os"
	"strings"
	"testing"

	dbpkg "github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestStatsSiteModelHourlyUpsertSQL(t *testing.T) {
	row := testStatsSiteModelHourlyRow()

	tests := []struct {
		name       string
		open       func(*testing.T) *gorm.DB
		contains   []string
		notContain []string
	}{
		{
			name: "postgres",
			open: openPostgresDryRunDB,
			contains: []string{
				`"date"="excluded"."date"`,
				`"stats_site_model_hourlies"."input_token" + "excluded"."input_token"`,
				`GREATEST("stats_site_model_hourlies"."last_request_at", "excluded"."last_request_at")`,
			},
			notContain: []string{`"date"="date"`, `MAX(`},
		},
		{
			name: "mysql",
			open: openMySQLDryRunDB,
			contains: []string{
				"`date`=VALUES(`date`)",
				"`stats_site_model_hourlies`.`input_token` + VALUES(`input_token`)",
				"GREATEST(`stats_site_model_hourlies`.`last_request_at`, VALUES(`last_request_at`))",
			},
			notContain: []string{"EXCLUDED", "`excluded`"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbConn := tt.open(t)
			result := dbConn.Clauses(statsSiteModelHourlyOnConflict(dbConn.Dialector.Name())).Create(&[]model.StatsSiteModelHourly{row})
			if result.Error != nil {
				t.Fatalf("build %s upsert: %v", tt.name, result.Error)
			}
			sql := result.Statement.SQL.String()
			for _, fragment := range tt.contains {
				if !strings.Contains(sql, fragment) {
					t.Fatalf("%s SQL missing %q:\n%s", tt.name, fragment, sql)
				}
			}
			for _, fragment := range tt.notContain {
				if strings.Contains(sql, fragment) {
					t.Fatalf("%s SQL unexpectedly contains %q:\n%s", tt.name, fragment, sql)
				}
			}
		})
	}
}

func TestSaveStatsSiteModelHourlyRowsSQLiteAccumulates(t *testing.T) {
	ctx := setupSiteOpTestDB(t)
	dbConn := dbpkg.GetDB().WithContext(ctx)

	first := testStatsSiteModelHourlyRow()
	second := first
	second.Date = "20260805"
	second.LastRequestAt = first.LastRequestAt - 10
	second.StatsMetrics = model.StatsMetrics{
		InputToken:     11,
		OutputToken:    12,
		InputCost:      1.25,
		OutputCost:     2.5,
		WaitTime:       13,
		RequestSuccess: 14,
		RequestFailed:  15,
	}

	if err := saveStatsSiteModelHourlyRows(dbConn, []model.StatsSiteModelHourly{first}); err != nil {
		t.Fatalf("insert first row: %v", err)
	}
	if err := saveStatsSiteModelHourlyRows(dbConn, []model.StatsSiteModelHourly{second}); err != nil {
		t.Fatalf("upsert second row: %v", err)
	}

	var got model.StatsSiteModelHourly
	if err := dbConn.First(&got).Error; err != nil {
		t.Fatalf("load accumulated row: %v", err)
	}
	assertStatsSiteModelHourlyAccumulated(t, got, first, second)
}

func TestStatsSiteModelHourlySaveDBRestoresCacheOnError(t *testing.T) {
	ctx := setupSiteOpTestDB(t)
	resetSiteModelHourlyCache(t)

	row := testStatsSiteModelHourlyRow()
	key := siteModelHourlyKey{
		Hour:          row.Hour,
		SiteAccountID: row.SiteAccountID,
		GroupKey:      row.GroupKey,
		ModelName:     row.ModelName,
	}
	siteModelHourlyCacheLock.Lock()
	siteModelHourlyCache[key] = &row
	siteModelHourlyCacheLock.Unlock()

	if err := dbpkg.GetDB().WithContext(ctx).Migrator().DropTable(&model.StatsSiteModelHourly{}); err != nil {
		t.Fatalf("drop stats table: %v", err)
	}
	if err := StatsSiteModelHourlySaveDB(ctx); err == nil {
		t.Fatal("expected save error after dropping stats table")
	}

	siteModelHourlyCacheLock.Lock()
	restored, ok := siteModelHourlyCache[key]
	siteModelHourlyCacheLock.Unlock()
	if !ok {
		t.Fatal("failed row was not restored to the hourly cache")
	}
	if *restored != row {
		t.Fatalf("restored row mismatch: got %+v, want %+v", *restored, row)
	}
}

func TestRestoreStatsSiteModelHourlyRowsMergesConcurrentUpdates(t *testing.T) {
	resetSiteModelHourlyCache(t)

	failed := testStatsSiteModelHourlyRow()
	key := siteModelHourlyKey{
		Hour:          failed.Hour,
		SiteAccountID: failed.SiteAccountID,
		GroupKey:      failed.GroupKey,
		ModelName:     failed.ModelName,
	}
	current := failed
	current.Date = "20260805"
	current.LastRequestAt = failed.LastRequestAt + 10
	current.StatsMetrics = model.StatsMetrics{RequestFailed: 3}
	siteModelHourlyCacheLock.Lock()
	siteModelHourlyCache[key] = &current
	siteModelHourlyCacheLock.Unlock()

	restoreStatsSiteModelHourlyRows([]model.StatsSiteModelHourly{failed})

	siteModelHourlyCacheLock.Lock()
	got := *siteModelHourlyCache[key]
	siteModelHourlyCacheLock.Unlock()
	if got.RequestSuccess != failed.RequestSuccess || got.RequestFailed != 3+failed.RequestFailed {
		t.Fatalf("restored metrics were not merged: %+v", got.StatsMetrics)
	}
	if got.LastRequestAt != current.LastRequestAt || got.Date != current.Date {
		t.Fatalf("newer concurrent metadata was overwritten: %+v", got)
	}
}

func TestStatsSiteModelHourlyUpsertPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("OCTOPUS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OCTOPUS_TEST_POSTGRES_DSN is not set")
	}

	dbConn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	sqlDB, err := dbConn.DB()
	if err != nil {
		t.Fatalf("get PostgreSQL connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	tx := dbConn.WithContext(context.Background()).Begin()
	if tx.Error != nil {
		t.Fatalf("begin PostgreSQL transaction: %v", tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })

	if err := tx.Exec(`CREATE TEMPORARY TABLE stats_site_model_hourlies (
		hour BIGINT NOT NULL,
		site_account_id BIGINT NOT NULL,
		group_key VARCHAR(128) NOT NULL,
		model_name VARCHAR(128) NOT NULL,
		date VARCHAR(8) NOT NULL,
		last_request_at BIGINT NOT NULL DEFAULT 0,
		input_token BIGINT,
		output_token BIGINT,
		input_cost REAL,
		output_cost REAL,
		wait_time BIGINT,
		request_success BIGINT,
		request_failed BIGINT,
		PRIMARY KEY (hour, site_account_id, group_key, model_name)
	) ON COMMIT DROP`).Error; err != nil {
		t.Fatalf("create temporary PostgreSQL table: %v", err)
	}

	first := testStatsSiteModelHourlyRow()
	second := first
	second.Date = "20260805"
	second.LastRequestAt = first.LastRequestAt - 10
	second.StatsMetrics = model.StatsMetrics{
		InputToken:     11,
		OutputToken:    12,
		InputCost:      1.25,
		OutputCost:     2.5,
		WaitTime:       13,
		RequestSuccess: 14,
		RequestFailed:  15,
	}

	if err := saveStatsSiteModelHourlyRows(tx, []model.StatsSiteModelHourly{first}); err != nil {
		t.Fatalf("insert first PostgreSQL row: %v", err)
	}
	if err := saveStatsSiteModelHourlyRows(tx, []model.StatsSiteModelHourly{second}); err != nil {
		t.Fatalf("upsert second PostgreSQL row: %v", err)
	}

	var got model.StatsSiteModelHourly
	if err := tx.First(&got).Error; err != nil {
		t.Fatalf("load accumulated PostgreSQL row: %v", err)
	}
	assertStatsSiteModelHourlyAccumulated(t, got, first, second)
}

func openPostgresDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbConn, err := gorm.Open(postgres.Open("host=127.0.0.1 user=octopus dbname=octopus port=1 sslmode=disable"), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Discard,
	})
	if err != nil {
		t.Fatalf("open dry-run PostgreSQL dialector: %v", err)
	}
	return dbConn
}

func openMySQLDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbConn, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "octopus:octopus@tcp(127.0.0.1:1)/octopus?parseTime=true",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Discard,
	})
	if err != nil {
		t.Fatalf("open dry-run MySQL dialector: %v", err)
	}
	return dbConn
}

func testStatsSiteModelHourlyRow() model.StatsSiteModelHourly {
	return model.StatsSiteModelHourly{
		Hour:          496000,
		SiteAccountID: 42,
		GroupKey:      "default",
		ModelName:     "test-model",
		Date:          "20260804",
		LastRequestAt: 1785772800,
		StatsMetrics: model.StatsMetrics{
			InputToken:     1,
			OutputToken:    2,
			InputCost:      0.25,
			OutputCost:     0.5,
			WaitTime:       3,
			RequestSuccess: 4,
			RequestFailed:  5,
		},
	}
}

func assertStatsSiteModelHourlyAccumulated(
	t *testing.T,
	got model.StatsSiteModelHourly,
	first model.StatsSiteModelHourly,
	second model.StatsSiteModelHourly,
) {
	t.Helper()
	if got.Date != second.Date {
		t.Fatalf("date = %q, want incoming date %q", got.Date, second.Date)
	}
	if got.LastRequestAt != first.LastRequestAt {
		t.Fatalf("last_request_at = %d, want max %d", got.LastRequestAt, first.LastRequestAt)
	}
	want := first.StatsMetrics
	want.Add(second.StatsMetrics)
	if got.StatsMetrics != want {
		t.Fatalf("accumulated metrics = %+v, want %+v", got.StatsMetrics, want)
	}
}

func resetSiteModelHourlyCache(t *testing.T) {
	t.Helper()
	siteModelHourlyCacheLock.Lock()
	siteModelHourlyCache = make(map[siteModelHourlyKey]*model.StatsSiteModelHourly)
	siteModelHourlyCacheLock.Unlock()
	t.Cleanup(func() {
		siteModelHourlyCacheLock.Lock()
		siteModelHourlyCache = make(map[siteModelHourlyKey]*model.StatsSiteModelHourly)
		siteModelHourlyCacheLock.Unlock()
	})
}

package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestMigrateAutoVacuumErrorPaths verifies that migrateAutoVacuum
// handles errors from each SQL operation gracefully: logs a WARNING and
// returns without panicking (issue #682).
func TestMigrateAutoVacuumErrorPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mode        int                   // auto_vacuum mode returned by mock (2=INCREMENTAL skips migration)
		setupMock   func(sqlmock.Sqlmock) // expectations per error case
		wantWarning string                // substring that must appear in the logged WARNING
	}{
		{
			name: "QueryRowContext_error",
			mode: 0, // NONE — triggers the migration path
			setupMock: func(mock sqlmock.Sqlmock) {
				// Simulate QueryRowContext itself returning an error.
				mock.ExpectQuery("PRAGMA auto_vacuum").
					WillReturnError(errors.New("query execution error"))
			},
			wantWarning: "WARN: metrics: check auto_vacuum mode: query execution error",
		},
		{
			name: "ExecContext_PRAGMA_INCREMENTAL_error",
			mode: 0, // NONE — triggers the PRAGMA auto_vacuum=INCREMENTAL step
			setupMock: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"mode"}).AddRow(0)
				mock.ExpectQuery("PRAGMA auto_vacuum").WillReturnRows(rows)
				// Simulate PRAGMA auto_vacuum=INCREMENTAL failing.
				mock.ExpectExec("PRAGMA auto_vacuum=INCREMENTAL").
					WillReturnError(errors.New("set pragma error"))
			},
			wantWarning: "WARN: metrics: set auto_vacuum=INCREMENTAL: set pragma error",
		},
		{
			name: "ExecContext_VACUUM_error",
			mode: 0, // NONE — triggers the VACUUM step
			setupMock: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"mode"}).AddRow(0)
				mock.ExpectQuery("PRAGMA auto_vacuum").WillReturnRows(rows)
				mock.ExpectExec("PRAGMA auto_vacuum=INCREMENTAL").
					WillReturnResult(sqlmock.NewResult(0, 0))
				// Simulate VACUUM failing.
				mock.ExpectExec("VACUUM").
					WillReturnError(errors.New("vacuum error"))
			},
			wantWarning: "WARN: metrics: VACUUM for auto_vacuum migration: vacuum error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })

			// Capture the logged warning.
			var loggedWarning string
			logger := func(format string, args ...any) {
				loggedWarning = fmt.Sprintf(format, args...)
			}

			tt.setupMock(mock)

			// The critical assertion: migrateAutoVacuum must not panic.
			migrateAutoVacuum(context.Background(), db, logger)

			// Verify the expected WARNING was logged.
			if !strings.Contains(loggedWarning, tt.wantWarning) {
				t.Errorf("logged warning %q does not contain expected substring %q",
					loggedWarning, tt.wantWarning)
			}
		})
	}
}

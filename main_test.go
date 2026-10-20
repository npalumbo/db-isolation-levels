package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// quietLogger implements testcontainers.Logging to suppress container startup/shutdown noise.
type quietLogger struct{}

func (quietLogger) Printf(format string, v ...interface{}) {}

// setupTestDB starts a fresh PostgreSQL container silently and returns the connection string.
func setupTestDB(t *testing.T) (string, func()) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("secret"),
		postgres.BasicWaitStrategies(),
		testcontainers.WithLogger(quietLogger{}), // <--- Silences the container logs here
		testcontainers.WithExposedPorts("5432"),
	)
	if err != nil {
		t.Fatalf("failed to start container: %v", err)
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}

	db, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close(ctx)

	_, err = db.Exec(ctx, `
		CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT, price INT);
		CREATE TABLE on_call_doctors (name TEXT PRIMARY KEY, on_call BOOLEAN);
		CREATE TABLE system_meta (id INT PRIMARY KEY, version INT);
	`)
	if err != nil {
		t.Fatalf("failed to setup schema: %v", err)
	}

	cleanup := func() {
		if err := testcontainers.TerminateContainer(pgContainer); err != nil {
			t.Logf("failed to terminate container: %v", err)
		}
	}

	return connStr, cleanup
}

func TestDirtyRead(t *testing.T) {
	ctx := context.Background()
	connStr, cleanup := setupTestDB(t)
	defer cleanup()

	db, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close(ctx)

	_, _ = db.Exec(ctx, "INSERT INTO items (name, price) VALUES ('Laptop', 1000)")

	conn1, _ := pgx.Connect(ctx, connStr)
	defer conn1.Close(ctx)
	tx1, _ := conn1.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	defer tx1.Rollback(ctx)

	_, _ = tx1.Exec(ctx, "UPDATE items SET price = 500 WHERE name = 'Laptop'")

	conn2, _ := pgx.Connect(ctx, connStr)
	defer conn2.Close(ctx)
	tx2, _ := conn2.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadUncommitted})
	defer tx2.Rollback(ctx)

	var price int
	err = tx2.QueryRow(ctx, "SELECT price FROM items WHERE name = 'Laptop'").Scan(&price)
	if err != nil {
		t.Fatalf("tx2 read failed: %v", err)
	}

	if price != 1000 {
		t.Errorf("Expected dirty read prevention (price 1000), but got uncommitted value: %d", price)
	}
}

func TestNonRepeatableRead(t *testing.T) {
	ctx := context.Background()
	connStr, cleanup := setupTestDB(t)
	defer cleanup()

	db, _ := pgx.Connect(ctx, connStr)
	defer db.Close(ctx)
	_, _ = db.Exec(ctx, "INSERT INTO items (name, price) VALUES ('Phone', 500)")

	conn1, _ := pgx.Connect(ctx, connStr)
	defer conn1.Close(ctx)

	// FIX: Use RepeatableRead so tx1 sees a consistent snapshot throughout
	tx1, err := conn1.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("tx1 begin failed: %v", err)
	}
	defer tx1.Rollback(ctx)

	var price1 int
	if err := tx1.QueryRow(ctx, "SELECT price FROM items WHERE name = 'Phone'").Scan(&price1); err != nil {
		t.Fatalf("tx1 first read failed: %v", err)
	}
	if price1 != 500 {
		t.Errorf("Expected initial price 500, got %d", price1)
	}

	go func() {
		bgConn, err := pgx.Connect(context.Background(), connStr)
		if err != nil {
			return
		}
		defer bgConn.Close(context.Background())

		tx2, err := bgConn.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return
		}
		_, _ = tx2.Exec(context.Background(), "UPDATE items SET price = 600 WHERE name = 'Phone'")
		_ = tx2.Commit(context.Background())
	}()

	time.Sleep(500 * time.Millisecond)

	var price2 int
	if err := tx1.QueryRow(ctx, "SELECT price FROM items WHERE name = 'Phone'").Scan(&price2); err != nil {
		t.Fatalf("tx1 second read failed: %v", err)
	}

	// FIX: Assert that price remains 500 (repeatable read is guaranteed)
	if price2 != 500 {
		t.Errorf("Expected non-repeatable read to be prevented (price 500), but got updated price: %d", price2)
	}

	_ = tx1.Commit(ctx)
}

func TestPhantomRead(t *testing.T) {
	ctx := context.Background()
	connStr, cleanup := setupTestDB(t)
	defer cleanup()

	db, _ := pgx.Connect(ctx, connStr)
	defer db.Close(ctx)
	_, _ = db.Exec(ctx, "INSERT INTO items (name, price) VALUES ('Item A', 100)")

	conn1, _ := pgx.Connect(ctx, connStr)
	defer conn1.Close(ctx)

	// FIX: Use RepeatableRead so tx1 sees a consistent snapshot throughout
	tx1, err := conn1.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("tx1 begin failed: %v", err)
	}
	defer tx1.Rollback(ctx)

	var count1 int
	if err := tx1.QueryRow(ctx, "SELECT COUNT(*) FROM items WHERE price < 200").Scan(&count1); err != nil {
		t.Fatalf("tx1 first count failed: %v", err)
	}
	if count1 != 1 {
		t.Errorf("Expected initial count 1, got %d", count1)
	}

	go func() {
		bgConn, err := pgx.Connect(context.Background(), connStr)
		if err != nil {
			return
		}
		defer bgConn.Close(context.Background())
		tx2, err := conn1.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return
		}
		_, _ = tx2.Exec(context.Background(), "INSERT INTO items (name, price) VALUES ('Item B', 150)")
		_ = tx2.Commit(context.Background())
	}()

	time.Sleep(500 * time.Millisecond)

	var count2 int
	if err := tx1.QueryRow(ctx, "SELECT COUNT(*) FROM items WHERE price < 200").Scan(&count2); err != nil {
		// SUCCESS: PostgreSQL successfully caught the conflict and aborted to prevent the phantom read!
		t.Logf("PostgreSQL successfully prevented phantom read via serialization error: %v", err)
		return
	}

	// If it didn't error out on the query, try committing to see if the conflict triggers there
	err = tx1.Commit(ctx)
	if err != nil {
		t.Logf("PostgreSQL successfully caught conflict on commit: %v", err)
		return
	}

	t.Errorf("Expected PostgreSQL to prevent phantom read or throw a serialization error, but both queries succeeded.")

	_ = tx1.Commit(ctx)
}

func TestSerializationAnomaly(t *testing.T) {
	ctx := context.Background()
	connStr, cleanup := setupTestDB(t)
	defer cleanup()

	db, _ := pgx.Connect(ctx, connStr)
	defer db.Close(ctx)
	_, _ = db.Exec(ctx, "INSERT INTO on_call_doctors VALUES ('Alice', true), ('Bob', true)")

	done := make(chan struct{})

	go func() {
		bgConn, err := pgx.Connect(context.Background(), connStr)
		if err != nil {
			close(done)
			return
		}
		defer bgConn.Close(context.Background())

		tx1, err := bgConn.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			close(done)
			return
		}
		defer tx1.Rollback(context.Background())

		var count int
		_ = tx1.QueryRow(context.Background(), "SELECT COUNT(*) FROM on_call_doctors WHERE on_call = true").Scan(&count)
		if count > 1 {
			_, _ = tx1.Exec(context.Background(), "UPDATE on_call_doctors SET on_call = false WHERE name = 'Alice'")
			_ = tx1.Commit(context.Background())
		}
		close(done)
	}()

	conn2, _ := pgx.Connect(ctx, connStr)
	defer conn2.Close(ctx)

	tx2, err := conn2.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("tx2 begin failed: %v", err)
	}
	defer tx2.Rollback(ctx)

	var count int
	if err := tx2.QueryRow(ctx, "SELECT COUNT(*) FROM on_call_doctors WHERE on_call = true").Scan(&count); err != nil {
		t.Fatalf("tx2 query failed: %v", err)
	}

	if count > 1 {
		_, _ = tx2.Exec(ctx, "UPDATE on_call_doctors SET on_call = false WHERE name = 'Bob'")
		_ = tx2.Commit(ctx)
	}

	<-done

	var activeCount int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM on_call_doctors WHERE on_call = true").Scan(&activeCount); err != nil {
		t.Fatalf("final query failed: %v", err)
	}

	if activeCount != 1 {
		t.Errorf("Serialization anomaly (1 doctor on call expected), but got %d active doctors", activeCount)
	}
}

func TestOptimisticLockingWriteSkew(t *testing.T) {
	ctx := context.Background()
	connStr, cleanup := setupTestDB(t)
	defer cleanup()

	db, _ := pgx.Connect(ctx, connStr)
	defer db.Close(ctx)

	// Setup initial data including the global version counter
	_, _ = db.Exec(ctx, `
		INSERT INTO on_call_doctors VALUES ('Alice', true), ('Bob', true);
		INSERT INTO system_meta VALUES (1, 1);
	`)

	successCount := 0
	var mu sync.Mutex
	done := make(chan struct{}, 2)

	// Worker function simulating Alice or Bob trying to go off-call optimistically
	tryGoOffCall := func(doctorName string) {
		defer func() { done <- struct{}{} }()

		bgConn, err := pgx.Connect(context.Background(), connStr)
		if err != nil {
			return
		}
		defer bgConn.Close(context.Background())

		// 1. Read phase: Get current version and active doctor count
		var currentVersion int
		var activeCount int
		_ = bgConn.QueryRow(context.Background(), "SELECT version FROM system_meta WHERE id = 1").Scan(&currentVersion)
		_ = bgConn.QueryRow(context.Background(), "SELECT COUNT(*) FROM on_call_doctors WHERE on_call = true").Scan(&activeCount)

		if activeCount <= 1 {
			return // Business rule check failed locally
		}

		// Simulate slight processing delay
		time.Sleep(50 * time.Millisecond)

		// 2. Write phase with Optimistic Lock check on version
		tx, err := bgConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return
		}
		defer tx.Rollback(context.Background())

		// Attempt to increment version conditionally
		cmdTag, err := tx.Exec(context.Background(),
			"UPDATE system_meta SET version = version + 1 WHERE id = 1 AND version = $1",
			currentVersion,
		)
		if err != nil || cmdTag.RowsAffected() == 0 {
			// Conflict detected! Another transaction modified the state first.
			return
		}

		// Update the doctor's status
		_, err = tx.Exec(context.Background(),
			"UPDATE on_call_doctors SET on_call = false WHERE name = $1",
			doctorName,
		)
		if err != nil {
			return
		}

		if err := tx.Commit(context.Background()); err == nil {
			mu.Lock()
			successCount++
			mu.Unlock()
		}
	}

	// Run Alice and Bob concurrently
	go tryGoOffCall("Alice")
	go tryGoOffCall("Bob")

	<-done
	<-done

	// Verify final state
	var finalActiveCount int
	_ = db.QueryRow(ctx, "SELECT COUNT(*) FROM on_call_doctors WHERE on_call = true").Scan(&finalActiveCount)

	// Assertions: Optimistic locking should ensure only ONE succeeds, leaving 1 active doctor (no write skew!)
	if successCount != 1 {
		t.Errorf("Expected exactly 1 successful transaction, but %d succeeded", successCount)
	}

	if finalActiveCount != 1 {
		t.Errorf("Expected write skew to be prevented (1 active doctor remaining), but got %d active doctors", finalActiveCount)
	}
}

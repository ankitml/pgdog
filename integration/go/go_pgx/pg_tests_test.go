package main

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
)

func assertNoOutOfSync(t *testing.T) {
	zero := pgtype.Numeric{
		Int:   big.NewInt(0),
		Exp:   0,
		NaN:   false,
		Valid: true,
	}

	conn, err := pgx.Connect(context.Background(), "postgres://admin:pgdog@127.0.0.1:6432/admin")
	if err != nil {
		panic(err)
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(), "SHOW POOLS", pgx.QueryExecModeSimpleProtocol)
	assert.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			panic(err)
		}

		for i, description := range rows.FieldDescriptions() {
			if description.Name == "out_of_sync" {
				out_of_sync := values[i].(pgtype.Numeric)
				assert.Equal(t, out_of_sync, zero, "No connections should be out of sync")
				return
			}
		}

		panic("No out_of_sync column in SHOW POOLS")
	}
}

func connectNormal() (*pgx.Conn, error) {
	conn, err := pgx.Connect(context.Background(), "postgres://pgdog:pgdog@127.0.0.1:6432/pgdog")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't connect: %v\n", err)
		return nil, err
	}

	return conn, nil
}

func connectAdmin() (*pgx.Conn, error) {
	conn, err := pgx.Connect(context.Background(), "postgres://admin:pgdog@127.0.0.1:6432/admin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't connect: %v\n", err)
		return nil, err
	}

	return conn, nil
}

func connectSharded() (*pgx.Conn, error) {
	conn, err := pgx.Connect(context.Background(), "postgres://pgdog:pgdog@127.0.0.1:6432/pgdog_sharded")

	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't connect: %v\n", err)
		return nil, err
	}

	return conn, nil
}

func connectBoth() []*pgx.Conn {
	// conns := make([]*pgx.Conn, 2)

	normal, err := connectNormal()
	if err != nil {
		panic(err)
	}
	sharded, err := connectSharded()
	if err != nil {
		panic(err)
	}

	conns := []*pgx.Conn{normal, sharded}

	return conns
}

func TestConnect(t *testing.T) {
	for _, conn := range connectBoth() {
		conn.Close(context.Background())
	}

	assertNoOutOfSync(t)
}

func TestSelect(t *testing.T) {
	conns := connectBoth()

	for _, conn := range conns {
		for i := range 25 {
			var one int64
			var len int
			rows, err := conn.Query(context.Background(), "SELECT $1::bigint AS one", i)
			if err != nil {
				panic(err)
			}

			for rows.Next() {
				len += 1
				values, err := rows.Values()

				if err != nil {
					panic(err)
				}

				one = values[0].(int64)
				assert.Equal(t, one, int64(i))
			}

			assert.Equal(t, len, 1)
		}

		conn.Close(context.Background())
	}

	assertNoOutOfSync(t)
}

func TestTimeout(t *testing.T) {
	c := make(chan int, 1)

	// Using 9 because the pool size is 10
	// and we're executing a slow query that will block
	// the pool for a while.
	// Test pool size is 10.
	for _ = range 9 {
		go func() {
			executeTimeoutTest(t)
			c <- 1
		}()
	}

	for _ = range 9 {
		<-c
	}

	// Wait for the conn to be drained and checked in
	time.Sleep(2 * time.Second)

}

func executeTimeoutTest(t *testing.T) {
	conns := connectBoth()

	for _, conn := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		c := make(chan int, 1)

		go func() {
			err := pgSleepOneSecond(conn, ctx)
			assert.NotNil(t, err)

			defer conn.Close(context.Background())

			c <- 0
		}()

		select {
		case <-c:
			t.Error("Context should of been cancelled")
		case <-ctx.Done():
		}
	}

}

// Sleep for 1 second.
func pgSleepOneSecond(conn *pgx.Conn, ctx context.Context) (err error) {
	_, err = conn.Exec(ctx, "SELECT pg_sleep(1)")
	return err
}

func TestCrud(t *testing.T) {
	conns := connectBoth()

	for _, conn := range conns {
		defer conn.Close(context.Background())
	}

	for _ = range 25 {
		for _, conn := range conns {
			id := rand.Intn(1_000_000)
			rows, err := conn.Query(context.Background(), "INSERT INTO sharded (id) VALUES ($1) RETURNING *", id)

			assert.Nil(t, err)

			for rows.Next() {
				values, err := rows.Values()
				assert.Nil(t, err)
				assert.Equal(t, int64(id), values[0].(int64))
			}

			rows, err = conn.Query(context.Background(), "SELECT * FROM sharded WHERE id = $1", id)

			var len int

			for rows.Next() {
				values, err := rows.Values()
				assert.Nil(t, err)
				len += 1
				assert.Equal(t, values[0].(int64), int64(id))
			}

			assert.Equal(t, len, 1)

			cmd, err := conn.Exec(context.Background(), "DELETE FROM sharded WHERE id = $1", id)
			assert.Nil(t, err)
			assert.True(t, cmd.Delete())
			assert.Equal(t, cmd.RowsAffected(), int64(1))
		}
	}
}

func TestTransactions(t *testing.T) {
	conns := connectBoth()

	for _, conn := range conns {
		defer conn.Close(context.Background())
	}

	for _ = range 25 {
		for _, conn := range conns {
			tx, err := conn.BeginTx(context.Background(), pgx.TxOptions{})
			assert.Nil(t, err)
			defer tx.Rollback(context.Background())

			id := rand.Intn(1_000_000)

			rows, err := tx.Query(context.Background(), "INSERT INTO sharded (id) VALUES ($1) RETURNING *", id)
			assert.Nil(t, err)
			var len int
			for rows.Next() {
				values, err := rows.Values()
				assert.Nil(t, err)
				assert.Equal(t, values[0].(int64), int64(id))
				len += 1
			}
			assert.Equal(t, len, 1)

			rows, err = tx.Query(context.Background(), "SELECT * FROM sharded WHERE id = $1", id)
			assert.Nil(t, err)
			len = 0
			for rows.Next() {
				values, err := rows.Values()
				assert.Nil(t, err)
				assert.Equal(t, values[0].(int64), int64(id))
				len += 1
			}
			assert.Equal(t, len, 1)
			err = tx.Rollback(context.Background())
			assert.Nil(t, err)
		}
	}
}

func TestBatch(t *testing.T) {
	conn, err := connectNormal()
	assert.NoError(t, err)

	tx, err := conn.Begin(context.Background())
	assert.NoError(t, err)

	tx.Exec(context.Background(), "SELECT * FROM (SELECT 1) t")

	batch := pgx.Batch{}
	batch.Queue("SELECT $1::integer", 1)
	batch.Queue("SELECT $1::integer, $2::integer", 1, 2)

	results := tx.SendBatch(context.Background(), &batch)

	for range 2 {
		rows, err := results.Query()
		assert.NoError(t, err)

		for rows.Next() {
			_, err := rows.Values()
			assert.NoError(t, err)
		}
		rows.Close()
	}

	results.Close()

	err = tx.Commit(context.Background())
	assert.NoError(t, err)

	tx2, err := conn.Begin(context.Background())
	assert.NoError(t, err)

	batch = pgx.Batch{}
	batch.Queue("SELECT $1::integer", 1)
	batch.Queue("SELECT $1::integer, $2::integer", 1, 2)

	results = tx2.SendBatch(context.Background(), &batch)

	tx.Commit(context.Background())
}

func TestLimitOffset(t *testing.T) {
	conns := connectBoth()

	for _, conn := range conns {
		defer conn.Close(context.Background())
	}

	for _, conn := range conns {
		_, err := conn.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS pgx_test_limit_offset(id BIGINT)")
		assert.NoError(t, err)
		defer conn.Exec(context.Background(), "DROP TABLE IF EXISTS pgx_test_limit_offset")

		_, err = conn.Exec(context.Background(), "SELECT * FROM pgx_test_limit_offset LIMIT $1 OFFSET $2", 5, 7)
		assert.NoError(t, err)

		_, err = conn.Exec(context.Background(), "SELECT * FROM pgx_test_limit_offset WHERE id = $1 LIMIT $2 OFFSET $3", 25, 6, 8)
		assert.NoError(t, err)

		_, err = conn.Exec(context.Background(), "SELECT * FROM pgx_test_limit_offset WHERE id = $1 LIMIT 25 OFFSET 50", 25)
		assert.NoError(t, err)
	}
}

func TestClosePrepared(t *testing.T) {
	conns := connectBoth()

	for _, conn := range conns {
		defer conn.Close(context.Background())
	}

	for _, conn := range conns {
		for range 25 {
			_, err := conn.Prepare(context.Background(), "test", "SELECT $1::bigint")
			assert.NoError(t, err)

			var one int64
			err = conn.QueryRow(context.Background(), "test", 1).Scan(&one)
			assert.NoError(t, err)
			assert.Equal(t, int64(1), one)

			err = conn.Deallocate(context.Background(), "test")
			assert.NoError(t, err)
		}
	}
}

func TestPreparedCounter(t *testing.T) {
	conn, err := pgx.Connect(context.Background(), "postgres://pgdog:pgdog@127.0.0.1:6432/pgdog?application_name=test_preapred_counter")
	assert.NoError(t, err)
	defer conn.Close(context.Background())

	admin, err := connectAdmin()
	assert.NoError(t, err)

	defer admin.Close(context.Background())

	for i := range 5 {
		var found bool
		name := fmt.Sprintf("test_%d", i)
		_, err := conn.Prepare(context.Background(), name, "SELECT $1::bigint")
		assert.NoError(t, err)

		rows, err := admin.Query(context.Background(), "SHOW CLIENTS prepared_statements, application_name", pgx.QueryExecModeSimpleProtocol)
		assert.NoError(t, err)
		defer rows.Close()

		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				panic(err)
			}

			var application_name string
			var prepared_statements pgtype.Numeric

			for i, description := range rows.FieldDescriptions() {
				if description.Name == "application_name" {
					application_name = values[i].(string)
				}

				if description.Name == "prepared_statements" {
					prepared_statements = values[i].(pgtype.Numeric)
				}
			}

			if application_name == "test_preapred_counter" {
				prepared := pgtype.Numeric{
					Int:   big.NewInt(int64(i + 1)),
					Exp:   0,
					NaN:   false,
					Valid: true,
				}

				assert.Equal(t, prepared, prepared_statements)
				found = true
			}
		}
		assert.True(t, found)
	}

	for i := range 5 {
		name := fmt.Sprintf("test_%d", i)
		conn.Deallocate(context.Background(), name)
	}

	var found bool

	rows, err := admin.Query(context.Background(), "SHOW CLIENTS prepared_statements, application_name", pgx.QueryExecModeSimpleProtocol)
	assert.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			panic(err)
		}

		var application_name string
		var prepared_statements pgtype.Numeric

		for i, description := range rows.FieldDescriptions() {
			if description.Name == "application_name" {
				application_name = values[i].(string)
			}

			if description.Name == "prepared_statements" {
				prepared_statements = values[i].(pgtype.Numeric)
			}
		}

		if application_name == "test_preapred_counter" {
			zero := pgtype.Numeric{
				Int:   big.NewInt(int64(0)),
				Exp:   0,
				NaN:   false,
				Valid: true,
			}

			assert.Equal(t, zero, prepared_statements)
			found = true
		}
	}
	assert.True(t, found)
}

func TestPreparedError(t *testing.T) {
	conn, err := connectNormal()
	assert.NoError(t, err)
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(), "SELECT $1::bigint, apples", 1)
	rows.Close()
	assert.Error(t, err)
}

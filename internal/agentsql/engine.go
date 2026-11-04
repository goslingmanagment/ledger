// Package agentsql answers the agents' SQL: POST /v1/sql runs one read-only statement and
// GET /v1/schema lists tables and columns with their Russian descriptions.
//
// Each guard alone stops writes; together they also stop the rest:
//   - its own pool opens the file with mode=ro and query_only;
//   - an SQLite authorizer on every connection compiles only SELECT, reads, functions and
//     recursive CTEs, so ATTACH, PRAGMA, transactions, DDL and writes fail to compile;
//   - SQLITE_LIMIT_ATTACHED is 0, a string or blob is at most 16 MB;
//   - one statement per request, cut before the driver, which would run a whole script;
//   - a deadline interrupts the statement at any step, and at most MaxRows rows come back.
package agentsql

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"time"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Defaults of an Engine.
const (
	DefaultTimeout  = 10 * time.Second
	DefaultMaxRows  = 10_000
	DefaultMaxBytes = 64 << 20 // rough size of one answer; past it the rows are cut as past MaxRows
	maxValue        = 16 << 20 // SQLITE_LIMIT_LENGTH: the longest string or blob
)

// Engine runs agents' statements on its own read-only pool.
type Engine struct {
	Timeout  time.Duration // the whole call, waiting for a connection included
	MaxRows  int
	MaxBytes int
	db       *sql.DB
}

// Result is what one statement returned.
type Result struct {
	Columns   []string
	Rows      [][]any // int64, float64, string, []byte, nil
	Truncated bool    // more rows than MaxRows (or MaxBytes) were there
}

// Kind says what is wrong with the agent's statement.
type Kind int

const (
	Invalid Kind = iota + 1 // not exactly one statement, or too long
	Denied                  // refused: only reading is allowed
	Timeout                 // interrupted at the deadline
	Failed                  // SQLite error: syntax, unknown table or column, …
)

// Error is a problem of the statement; any other error of Run is the service's own.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

var errDriver = errors.New("agentsql: the sqlite driver lacks the authorizer or the handle " +
	"to interrupt a statement; agent SQL stays off rather than run unguarded")

// drv is a driver of its own, so the policy below reaches only agent connections.
var drv = func() *sqlite.Driver {
	d := &sqlite.Driver{}
	d.RegisterConnectionHook(func(c sqlite.ExecQuerierContext, _ string) error {
		a, ok := c.(sqlite.AuthorizerRegisterer)
		if !ok {
			return errDriver
		}
		return a.RegisterAuthorizer(readOnly(nil))
	})
	return d
}()

type connector struct{ dsn string }

func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return drv.Open(c.dsn)
}

func (c connector) Driver() driver.Driver { return drv }

// Open prepares the pool for the database at path; connections open on first use.
func Open(path string) *Engine {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	db := sql.OpenDB(connector{dsn: "file:" + path + "?" + q.Encode()})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &Engine{Timeout: DefaultTimeout, MaxRows: DefaultMaxRows, MaxBytes: DefaultMaxBytes, db: db}
}

// Close closes the pool.
func (e *Engine) Close() error { return e.db.Close() }

// Run executes one read-only statement. Problems of the statement are *Error.
func (e *Engine) Run(ctx context.Context, query string) (*Result, error) {
	stmt, err := Statement(query)
	if err != nil {
		return nil, &Error{Kind: Invalid, Msg: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	conn, err := e.db.Conn(ctx)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, e.timeout()
		}
		return nil, err
	}
	defer conn.Close()
	for _, l := range [][2]int{{sqlite3.SQLITE_LIMIT_ATTACHED, 0}, {sqlite3.SQLITE_LIMIT_LENGTH, maxValue}} {
		if _, err := sqlite.Limit(conn, l[0], l[1]); err != nil {
			return nil, err
		}
	}
	var res *Result
	var refused denial
	err = conn.Raw(func(dc any) (err error) {
		res, err = e.run(ctx, dc, stmt, &refused)
		return err
	})
	switch {
	case err == nil:
		return res, nil
	case refused.set:
		return nil, &Error{Kind: Denied, Msg: refused.message(stmt)}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, e.timeout()
	case ctx.Err() != nil:
		return nil, ctx.Err() // the caller has gone
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return nil, err
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_AUTH, sqlite3.SQLITE_READONLY:
		return nil, &Error{Kind: Denied, Msg: deniedMsg + ": " + se.Error()}
	}
	return nil, &Error{Kind: Failed, Msg: se.Error()}
}

func (e *Engine) timeout() *Error {
	return &Error{Kind: Timeout, Msg: fmt.Sprintf("запрос прерван: дольше %s", e.Timeout)}
}

func (e *Engine) run(ctx context.Context, dc any, stmt string, refused *denial) (*Result, error) {
	authz, ok := dc.(sqlite.AuthorizerRegisterer)
	q, ok2 := dc.(driver.QueryerContext)
	interrupt := interrupter(dc)
	if !ok || !ok2 || interrupt == nil {
		return nil, errDriver
	}
	if err := authz.RegisterAuthorizer(readOnly(refused)); err != nil {
		return nil, err
	}
	defer authz.RegisterAuthorizer(readOnly(nil))

	// The driver interrupts on ctx only while QueryContext runs, that is the first step; the
	// steps of rows.Next run without it, so a query whose next row never comes would hold its
	// connection forever. The watchdog interrupts any step and stands down before the
	// connection goes back to the pool.
	var mu sync.Mutex
	live := true
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if live {
			interrupt()
		}
	})
	defer func() {
		stop()
		mu.Lock()
		live = false
		mu.Unlock()
	}()

	rows, err := q.QueryContext(ctx, stmt, nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := &Result{Columns: rows.Columns(), Rows: [][]any{}}
	dest := make([]driver.Value, len(res.Columns))
	size := 0
	for {
		if err := rows.Next(dest); err != nil {
			if errors.Is(err, io.EOF) {
				return res, nil
			}
			return nil, err
		}
		if len(res.Rows) >= e.MaxRows || size > e.MaxBytes {
			res.Truncated = true
			return res, nil
		}
		row := make([]any, len(dest))
		for i, v := range dest {
			var n int
			row[i], n = jsonValue(v)
			size += n
		}
		res.Rows = append(res.Rows, row)
	}
}

// interrupter returns sqlite3_interrupt for the connection, done the way the driver does it on
// a cancelled context: a fresh TLS, since the running query owns the connection's own. The
// handle is an unexported field of the modernc connection; if a driver update moves it, the
// result is nil and agent SQL refuses to run instead of running without a deadline.
func interrupter(dc any) func() {
	v := reflect.ValueOf(dc)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return nil
	}
	f := v.Elem().FieldByName("db")
	if !f.IsValid() || f.Kind() != reflect.Uintptr || f.Uint() == 0 {
		return nil
	}
	handle := uintptr(f.Uint())
	return func() {
		tls := libc.NewTLS()
		sqlite3.Xsqlite3_interrupt(tls, handle)
		tls.Close()
	}
}

// jsonValue copies a driver value (the driver may reuse its buffers) into something
// encoding/json writes faithfully, with a rough size of what it writes.
func jsonValue(v driver.Value) (any, int) {
	switch x := v.(type) {
	case nil:
		return nil, 4
	case string:
		return x, len(x) + 2
	case []byte:
		return bytes.Clone(x), len(x)*4/3 + 4 // base64 in JSON
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) { // JSON has no Inf
			return strconv.FormatFloat(x, 'g', -1, 64), 6
		}
		return x, 8
	case time.Time:
		return x.Format(time.RFC3339Nano), 32
	}
	return v, 8
}

// readOnly is the policy of every agent connection: SELECT, reading columns, calling
// functions and recursive CTEs compile, anything else is refused, and so is the column with the keys'
// hashes. The first refusal goes to d.
func readOnly(d *denial) sqlite.AuthorizerFn {
	return func(action sqlite.AuthorizerActionCode, arg1, arg2, _, _ string) sqlite.AuthorizerReturnCode {
		switch action {
		case sqlite.AuthSelect, sqlite.AuthFunction, sqlite.AuthRecursive:
			return sqlite.AuthorizerOK
		case sqlite.AuthRead:
			if arg1 != "keys" || arg2 != "hash" {
				return sqlite.AuthorizerOK
			}
		}
		if d != nil && !d.set {
			*d = denial{set: true, action: action, arg1: arg1, arg2: arg2}
		}
		return sqlite.AuthorizerDeny
	}
}

const deniedMsg = "запрещено: ключ агента только читает (SELECT)"

// denial is the first operation the authorizer refused.
type denial struct {
	set        bool
	action     sqlite.AuthorizerActionCode
	arg1, arg2 string
}

var actionNames = map[sqlite.AuthorizerActionCode]string{
	sqlite.AuthCreateIndex: "CREATE INDEX", sqlite.AuthCreateTable: "CREATE TABLE",
	sqlite.AuthCreateTempIndex: "CREATE TEMP INDEX", sqlite.AuthCreateTempTable: "CREATE TEMP TABLE",
	sqlite.AuthCreateTempTrigger: "CREATE TEMP TRIGGER", sqlite.AuthCreateTempView: "CREATE TEMP VIEW",
	sqlite.AuthCreateTrigger: "CREATE TRIGGER", sqlite.AuthCreateView: "CREATE VIEW",
	sqlite.AuthCreateVTable: "CREATE VIRTUAL TABLE", sqlite.AuthDropIndex: "DROP INDEX",
	sqlite.AuthDropTable: "DROP TABLE", sqlite.AuthDropTempIndex: "DROP TEMP INDEX",
	sqlite.AuthDropTempTable: "DROP TEMP TABLE", sqlite.AuthDropTempTrigger: "DROP TEMP TRIGGER",
	sqlite.AuthDropTempView: "DROP TEMP VIEW", sqlite.AuthDropTrigger: "DROP TRIGGER",
	sqlite.AuthDropView: "DROP VIEW", sqlite.AuthDropVTable: "DROP VIRTUAL TABLE",
	sqlite.AuthAlterTable: "ALTER TABLE", sqlite.AuthInsert: "INSERT", sqlite.AuthUpdate: "UPDATE",
	sqlite.AuthDelete: "DELETE", sqlite.AuthPragma: "PRAGMA", sqlite.AuthAttach: "ATTACH",
	sqlite.AuthDetach: "DETACH", sqlite.AuthReindex: "REINDEX", sqlite.AuthAnalyze: "ANALYZE",
	sqlite.AuthSavepoint: "SAVEPOINT", sqlite.AuthTransaction: "транзакция",
}

// schemaTables are the names SQLite gives the authorizer when DDL writes the schema: it asks
// about that write before CREATE, DROP or ANALYZE itself, so the statement's verb says more.
var schemaTables = map[string]bool{"sqlite_master": true, "sqlite_temp_master": true,
	"sqlite_schema": true, "sqlite_temp_schema": true}

func (d denial) message(stmt string) string {
	if d.action == sqlite.AuthRead {
		return "запрещено: хеши ключей агентам не видны; отклонено: чтение " + d.arg1 + "." + d.arg2
	}
	what, ok := actionNames[d.action]
	if !ok {
		what = "операция " + strconv.Itoa(int(d.action))
	}
	switch {
	case schemaTables[d.arg1] && (d.action == sqlite.AuthInsert || d.action == sqlite.AuthUpdate || d.action == sqlite.AuthDelete):
		what = "изменение схемы"
		if v := verb(stmt); v != "" {
			what = v + ", " + what
		}
	case d.action == sqlite.AuthTransaction:
		what += " " + d.arg1 // BEGIN, COMMIT, ROLLBACK
	case d.action == sqlite.AuthPragma && d.arg2 != "":
		what += " " + d.arg1 + " " + d.arg2
	case d.arg1 != "":
		what += " " + d.arg1
	}
	return deniedMsg + "; отклонено: " + what
}

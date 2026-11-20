package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	slugRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)
	memoryKeyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
)

func validScope(scope string) bool {
	for _, prefix := range []string{"game:", "app:"} {
		if name, ok := strings.CutPrefix(scope, prefix); ok {
			return slugRE.MatchString(name)
		}
	}
	return deviceIDRE.MatchString(scope)
}

type memoryWrite struct{ scope, key, value string }

func parseMemoryWrite(args object) (memoryWrite, error) {
	scope, key := str(args["scope"]), str(args["key"])
	if !validScope(scope) || !memoryKeyRE.MatchString(key) {
		return memoryWrite{}, problem(400, "Invalid memory scope or key")
	}
	value, ok := args["value"]
	if !ok {
		return memoryWrite{}, problem(400, "Memory value is required (may be null)")
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 16384 {
		return memoryWrite{}, problem(400, "Memory value exceeds 16384 JSON bytes")
	}
	return memoryWrite{scope, key, string(raw)}, nil
}

type versionConflict struct{ version int64 }

func (e *versionConflict) Error() string { return "version_conflict" }

func (d *bridgeDB) setMemory(ctx context.Context, w memoryWrite, expected *uint64) (int64, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var current int64
	err = tx.QueryRowContext(ctx, "SELECT version FROM memory WHERE scope=? AND key=?", w.scope, w.key).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if expected != nil && uint64(current) != *expected {
		return 0, &versionConflict{current}
	}
	now := time.Now().UnixMilli()
	var result sql.Result
	if current == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO memory (scope,key,value_json,updated_at)
			SELECT ?,?,?,? WHERE (SELECT count(*) FROM memory WHERE scope=?) < 64
			ON CONFLICT(scope,key) DO NOTHING`, w.scope, w.key, w.value, now, w.scope)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE memory SET value_json=?,version=version+1,updated_at=?
			WHERE scope=? AND key=? AND version=?`, w.value, now, w.scope, w.key, current)
	}
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed == 0 {
		return 0, problem(409, "Memory scope already has 64 keys")
	}
	return current + 1, tx.Commit()
}

func (b *bridge) memory(ctx context.Context, args object, values bool) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Memory unavailable")
	}
	op, scope, key := str(args["op"]), str(args["scope"]), str(args["key"])
	if !validScope(scope) || op != "list" && !memoryKeyRE.MatchString(key) {
		return nil, problem(400, "Invalid memory scope or key")
	}
	dbError := problem(502, "Cannot access saved memory")
	switch op {
	case "list":
		rows, err := b.db.QueryContext(ctx, "SELECT key,value_json,version,updated_at FROM memory WHERE scope=? ORDER BY key", scope)
		if err != nil {
			return nil, dbError
		}
		defer rows.Close()
		entries := []object{}
		for rows.Next() {
			var k, raw string
			var version, at int64
			if err := rows.Scan(&k, &raw, &version, &at); err != nil {
				return nil, dbError
			}
			entry := object{"key": k, "version": version, "updated_at": at}
			if values {
				entry["value"] = json.RawMessage(raw)
			}
			entries = append(entries, entry)
		}
		if rows.Err() != nil {
			return nil, dbError
		}
		return object{"scope": scope, "entries": entries}, nil
	case "get":
		var raw string
		var version int64
		err := b.db.QueryRowContext(ctx, "SELECT value_json,version FROM memory WHERE scope=? AND key=?", scope, key).Scan(&raw, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return object{"scope": scope, "key": key, "found": false}, nil
		}
		if err != nil {
			return nil, dbError
		}
		return object{"scope": scope, "key": key, "found": true, "version": version, "value": json.RawMessage(raw)}, nil
	case "set":
		write, err := parseMemoryWrite(args)
		if err != nil {
			return nil, err
		}
		expected, err := mcpUint(args, "expected_version")
		if err != nil {
			return nil, err
		}
		version, err := b.db.setMemory(ctx, write, expected)
		var conflict *versionConflict
		var api *apiError
		if err != nil {
			if errors.As(err, &conflict) || errors.As(err, &api) {
				return nil, err
			}
			return nil, dbError
		}
		return object{"ok": true, "scope": scope, "key": key, "version": version}, nil
	case "delete":
		expected, err := mcpUint(args, "expected_version")
		if err != nil {
			return nil, err
		}
		deleted, err := b.db.deleteMemory(ctx, scope, key, expected)
		if err != nil {
			var conflict *versionConflict
			if errors.As(err, &conflict) {
				return nil, err
			}
			return nil, dbError
		}
		return object{"ok": true, "scope": scope, "key": key, "deleted": deleted}, nil
	default:
		return nil, problem(400, "Unknown memory operation")
	}
}

func (d *bridgeDB) deleteMemory(ctx context.Context, scope, key string, expected *uint64) (bool, error) {
	if expected == nil {
		return deleteRow(ctx, d, "DELETE FROM memory WHERE scope=? AND key=?", scope, key)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var current int64
	err = tx.QueryRowContext(ctx, "SELECT version FROM memory WHERE scope=? AND key=?", scope, key).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if uint64(current) != *expected {
		return false, &versionConflict{current}
	}
	if current == 0 {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM memory WHERE scope=? AND key=? AND version=?", scope, key, current)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n != 0, tx.Commit()
}

func deleteRow(ctx context.Context, d *bridgeDB, query string, args ...any) (bool, error) {
	result, err := d.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n != 0, err
}

func (b *bridge) memoryAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	if b.db == nil {
		return nil, problem(503, "Memory unavailable")
	}
	q := r.URL.Query()
	args := object{"scope": q.Get("scope"), "key": q.Get("key")}
	switch {
	case r.Method == http.MethodGet && path == "/api/memory":
		args["op"] = "list"
		return b.memory(r.Context(), args, true)
	case r.Method == http.MethodDelete && path == "/api/memory":
		args["op"] = "delete"
		_, err := b.memory(r.Context(), args, false)
		if err == nil {
			w.WriteHeader(204)
		}
		return nil, err
	case r.Method == http.MethodGet && path == "/api/memory/scopes":
		dbError := problem(502, "Cannot list memory scopes")
		rows, err := b.db.QueryContext(r.Context(), "SELECT scope,count(*) FROM memory GROUP BY scope ORDER BY scope")
		if err != nil {
			return nil, dbError
		}
		defer rows.Close()
		scopes := []object{}
		for rows.Next() {
			var scope string
			var count int
			if err := rows.Scan(&scope, &count); err != nil {
				return nil, dbError
			}
			scopes = append(scopes, object{"scope": scope, "count": count})
		}
		if rows.Err() != nil {
			return nil, dbError
		}
		return object{"scopes": scopes}, nil
	}
	return nil, problem(404, "Not found")
}

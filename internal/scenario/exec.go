package scenario

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/segmentio/kafka-go"

	"github.com/ddlg3003/d-mock/internal/config"
	"github.com/ddlg3003/d-mock/internal/mock"
	"github.com/ddlg3003/d-mock/internal/tmpl"
)

func str(args map[string]any, k string) string { return tmpl.Stringify(args[k]) }

func strList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, len(t))
		for i, e := range t {
			out[i] = tmpl.Stringify(e)
		}
		return out
	case string:
		var out []string
		for _, s := range strings.Split(t, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return []string{tmpl.Stringify(v)}
}

func parseJSON(s string) (any, bool) {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return nil, false
	}
	// UseNumber keeps big ids exact (a float64 would round 64-bit ids and print 2.5e+14)
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	return v, true
}

// ---- http ----------------------------------------------------------------

// http: {method, url, headers, body (text) | json (object), timeout}
// result: {status, body, json, headers}
func execHTTP(ctx context.Context, a map[string]any) (map[string]any, error) {
	method := strings.ToUpper(str(a, "method"))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	headers := map[string]string{}
	if j, ok := a["json"]; ok {
		b, err := json.Marshal(j)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
		headers["Content-Type"] = "application/json"
	} else if b, ok := a["body"]; ok {
		body = strings.NewReader(tmpl.Stringify(b))
	}
	timeout := 30 * time.Second
	if t := str(a, "timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, err
		}
		timeout = d
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, str(a, "url"), body)
	if err != nil {
		return nil, err
	}
	if h, ok := a["headers"].(map[string]any); ok {
		for k, v := range h {
			headers[k] = tmpl.Stringify(v)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{"status": resp.StatusCode, "body": string(b)}
	if j, ok := parseJSON(string(b)); ok {
		out["json"] = j
	}
	hs := map[string]any{}
	for k := range resp.Header {
		hs[strings.ToLower(k)] = resp.Header.Get(k)
	}
	out["headers"] = hs
	return out, nil
}

// ---- exec ----------------------------------------------------------------

// exec: {cmd: "shell line" | [argv], dir, env: {}, stdin, timeout}
// result: {exit, stdout, stderr, json (stdout parsed when it is JSON)}
func execCmd(ctx context.Context, a map[string]any, baseDir string) (map[string]any, error) {
	timeout := 5 * time.Minute
	if t := str(a, "timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, err
		}
		timeout = d
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch c := a["cmd"].(type) {
	case string:
		cmd = exec.CommandContext(cctx, "sh", "-c", c)
	case []any:
		argv := strList(c)
		if len(argv) == 0 {
			return nil, errors.New("exec: empty cmd")
		}
		cmd = exec.CommandContext(cctx, argv[0], argv[1:]...)
	default:
		return nil, errors.New("exec: cmd must be a string or a list")
	}
	cmd.Dir = baseDir
	if d := str(a, "dir"); d != "" {
		cmd.Dir = d
	}
	cmd.Env = os.Environ()
	if e, ok := a["env"].(map[string]any); ok {
		for k, v := range e {
			cmd.Env = append(cmd.Env, k+"="+tmpl.Stringify(v))
		}
	}
	if s, ok := a["stdin"]; ok {
		cmd.Stdin = strings.NewReader(tmpl.Stringify(s))
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	exit := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	} else if err != nil {
		return nil, err
	}
	out := map[string]any{"exit": exit, "stdout": so.String(), "stderr": se.String()}
	if j, ok := parseJSON(so.String()); ok {
		out["json"] = j
	}
	return out, nil
}

// ---- sql -----------------------------------------------------------------

var (
	ddlRe      = regexp.MustCompile(`(?i)^\s*(create|alter|drop|truncate|rename|grant|revoke)\b`)
	writeRe    = regexp.MustCompile(`(?i)^\s*(insert|update|delete|replace)\b`)
	queryRe    = regexp.MustCompile(`(?i)^\s*(select|show|describe|desc|explain|with)\b`)
	writeKwRe  = regexp.MustCompile(`(?i)\b(insert|update|delete|replace|create|alter|drop|truncate|rename|grant|revoke|call|load|set|lock)\b`)
	selForUpRe = regexp.MustCompile(`(?i)\bfor\s+update\b`)
)

// splitSQL removes /* */, -- and # comments (outside quotes), so a comment can neither hide
// a statement's real kind nor carry the run id past the write guard. It returns the
// statement without comments, and a skeleton where every quoted literal is also emptied,
// so a value such as 'update' or '#1' is never read as a keyword or a comment.
func splitSQL(q string) (bare, skeleton string) {
	var b, k strings.Builder
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(q) && q[j] != c {
				if q[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(q) {
				j = len(q) - 1
			}
			b.WriteString(q[i : j+1])
			k.WriteByte(c)
			k.WriteByte(c)
			i = j
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				i = len(q)
			} else {
				i += end + 3
			}
			b.WriteByte(' ')
			k.WriteByte(' ')
		case c == '#' || (c == '-' && i+1 < len(q) && q[i+1] == '-'):
			for i < len(q) && q[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			k.WriteByte(' ')
		default:
			b.WriteByte(c)
			k.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String()), strings.TrimSpace(k.String())
}

// isReadOnly reports whether a statement only reads. Anything else counts as a write
// (default deny), including CALL, SET, LOAD DATA and a WITH … DELETE.
func isReadOnly(q string) bool {
	if !queryRe.MatchString(q) {
		return false
	}
	return !writeKwRe.MatchString(selForUpRe.ReplaceAllString(q, ""))
}

func (ru *run) dep(name, kind string) (map[string]any, *config.Dep, error) {
	d, ok := ru.r.Stack.Stack.Deps[name]
	if !ok {
		return nil, nil, fmt.Errorf("no dep named %q", name)
	}
	if d.Kind != kind {
		return nil, nil, fmt.Errorf("dep %s is kind %q, step needs %q", name, d.Kind, kind)
	}
	vars, _ := ru.data["deps"].(map[string]any)[name].(map[string]any)
	return vars, d, nil
}

// sql: {dep, query, args: []}  or  {dep, insert: {table, key, row: {}}}
// result: {rows: [{col: val}], count, affected}
// Rules: never DDL; on a remote dep every write must mention the run id, so a scenario can
// only touch rows it created. insert rows are deleted again during cleanup.
func (ru *run) execSQL(ctx context.Context, a map[string]any) (map[string]any, error) {
	vars, d, err := ru.dep(str(a, "dep"), "mysql")
	if err != nil {
		return nil, err
	}
	dsn := tmpl.Stringify(vars["dsn"])
	if dsn == "" {
		return nil, fmt.Errorf("dep %s has no dsn var", str(a, "dep"))
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if ins, ok := a["insert"].(map[string]any); ok {
		return ru.sqlInsert(ctx, db, str(a, "dep"), d, ins)
	}

	q := str(a, "query")
	qargs := []any{}
	if l, ok := a["args"].([]any); ok {
		qargs = l
	}
	bare, skel := splitSQL(q)
	if ddlRe.MatchString(skel) {
		return nil, fmt.Errorf("sql: DDL is not allowed: %s", firstLine(bare))
	}
	if strings.Contains(strings.TrimRight(skel, "; \t\n"), ";") {
		return nil, fmt.Errorf("sql: one statement per step: %s", firstLine(bare))
	}
	readOnly := isReadOnly(skel)
	if !readOnly && d.Mode == "remote" && !mentionsRun(ru.id, bare, qargs) {
		return nil, fmt.Errorf("sql: a write on remote dep %s must reference the run id %s (in the statement or args, not in a comment)", str(a, "dep"), ru.id)
	}
	if readOnly {
		rows, err := db.QueryContext(ctx, q, qargs...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list, err := scanRows(rows)
		if err != nil {
			return nil, err
		}
		return map[string]any{"rows": list, "count": len(list)}, nil
	}
	r, err := db.ExecContext(ctx, q, qargs...)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	return map[string]any{"affected": n}, nil
}

func (ru *run) sqlInsert(ctx context.Context, db *sql.DB, dep string, d *config.Dep, ins map[string]any) (map[string]any, error) {
	table, key := tmpl.Stringify(ins["table"]), tmpl.Stringify(ins["key"])
	row, _ := ins["row"].(map[string]any)
	if table == "" || key == "" || len(row) == 0 {
		return nil, errors.New("sql.insert needs table, key and row")
	}
	if !identRe.MatchString(table) || !identRe.MatchString(key) {
		return nil, errors.New("sql.insert: table and key must be plain identifiers")
	}
	keyVal := tmpl.Stringify(row[key])
	if !strings.Contains(keyVal, ru.id) {
		return nil, fmt.Errorf("sql.insert: key %s=%q must contain the run id %s", key, keyVal, ru.id)
	}
	cols := config.SortedKeys(row)
	ph := make([]string, len(cols))
	vals := make([]any, len(cols))
	quoted := make([]string, len(cols))
	for i, c := range cols {
		if !identRe.MatchString(c) {
			return nil, fmt.Errorf("sql.insert: bad column name %q", c)
		}
		quoted[i] = "`" + c + "`"
		ph[i] = "?"
		v := row[c]
		if m, ok := v.(map[string]any); ok { // JSON columns
			b, _ := json.Marshal(m)
			v = string(b)
		}
		vals[i] = v
	}
	q := fmt.Sprintf("INSERT INTO `%s` (%s) VALUES (%s)", table, strings.Join(quoted, ","), strings.Join(ph, ","))
	if _, err := db.ExecContext(ctx, q, vals...); err != nil {
		return nil, err
	}
	ru.ledger.add(undo{Kind: "sql", Dep: dep, Table: table, Key: key, Value: keyVal})
	return map[string]any{"affected": 1, "key": keyVal}, nil
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func mentionsRun(id, q string, args []any) bool {
	if strings.Contains(q, id) {
		return true
	}
	for _, a := range args {
		if strings.Contains(tmpl.Stringify(a), id) {
			return true
		}
	}
	return false
}

func scanRows(rows *sql.Rows) ([]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var list []any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				s := string(b)
				if j, ok := parseJSON(s); ok {
					m[c+"_json"] = j
				}
				v = s
			}
			m[c] = v
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// ---- kafka ---------------------------------------------------------------

// kafka: {dep, topic, key, value (text) | json (object), headers: {}}
// dep vars: brokers (list or comma separated)
func (ru *run) execKafka(ctx context.Context, a map[string]any) (map[string]any, error) {
	vars, _, err := ru.dep(str(a, "dep"), "kafka")
	if err != nil {
		return nil, err
	}
	brokers := strList(vars["brokers"])
	if len(brokers) == 0 {
		return nil, fmt.Errorf("dep %s has no brokers var", str(a, "dep"))
	}
	var value []byte
	if j, ok := a["json"]; ok {
		if value, err = json.Marshal(j); err != nil {
			return nil, err
		}
	} else {
		value = []byte(str(a, "value"))
	}
	msg := kafka.Message{Key: []byte(str(a, "key")), Value: value}
	if h, ok := a["headers"].(map[string]any); ok {
		for _, k := range config.SortedKeys(h) {
			msg.Headers = append(msg.Headers, kafka.Header{Key: k, Value: []byte(tmpl.Stringify(h[k]))})
		}
	}
	w := &kafka.Writer{Addr: kafka.TCP(brokers...), Topic: str(a, "topic"), Balancer: &kafka.Hash{}, RequiredAcks: kafka.RequireOne, WriteTimeout: 15 * time.Second}
	defer w.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := w.WriteMessages(cctx, msg); err != nil {
		return nil, err
	}
	return map[string]any{"topic": str(a, "topic"), "sent": 1}, nil
}

// ---- mock ----------------------------------------------------------------

// mock: {name, set: {key, state: {}}} | {name, fault: {key, fault, times}} | {name, clear: key}
//
//	| {name, reset: true} | {name, calls: {route, key}}
//
// calls result: {count, calls: [...]}
func (ru *run) execMock(a map[string]any) (map[string]any, error) {
	name := str(a, "name")
	admin, spec, err := ru.r.Stack.MockAdmin(name)
	if err != nil {
		return nil, err
	}
	switch {
	case a["set"] != nil:
		s, _ := a["set"].(map[string]any)
		key := keyOr(s["key"])
		fields := map[string]string{}
		if st, ok := s["state"].(map[string]any); ok {
			for k, v := range st {
				fields[k] = tmpl.Stringify(v)
			}
		}
		if err := spec.CheckState(fields); err != nil {
			return nil, fmt.Errorf("mock %s: %w", name, err)
		}
		if err := admin.SetState(key, fields); err != nil {
			return nil, err
		}
		ru.ledger.add(undo{Kind: "mock-state", Mock: name, Key: key, Fields: config.SortedKeys(fields)})
		return map[string]any{"key": key, "state": fields}, nil
	case a["fault"] != nil:
		f, _ := a["fault"].(map[string]any)
		key := keyOr(f["key"])
		times := 0
		if t, ok := f["times"]; ok {
			fmt.Sscan(tmpl.Stringify(t), &times)
		}
		if err := spec.CheckFault(str(f, "fault")); err != nil {
			return nil, fmt.Errorf("mock %s: %w", name, err)
		}
		if err := admin.SetFault(key, str(f, "fault"), times); err != nil {
			return nil, err
		}
		ru.ledger.add(undo{Kind: "mock-fault", Mock: name, Key: key})
		return map[string]any{"key": key, "fault": str(f, "fault"), "times": times}, nil
	case a["clear"] != nil:
		key := keyOr(a["clear"])
		return map[string]any{"key": key}, admin.SetFault(key, "", 0)
	case a["reset"] != nil:
		return map[string]any{"reset": true}, admin.Reset()
	case a["calls"] != nil:
		c, _ := a["calls"].(map[string]any)
		calls, err := admin.Calls(spec)
		if err != nil {
			return nil, err
		}
		list := []any{}
		for _, call := range mock.Filter(calls, str(c, "route"), str(c, "key")) {
			b, _ := json.Marshal(call)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if j, ok := parseJSON(call.Body); ok {
				m["json"] = j
			}
			list = append(list, m)
		}
		return map[string]any{"count": len(list), "calls": list}, nil
	}
	return nil, errors.New("mock step needs one of set, fault, clear, reset, calls")
}

func keyOr(v any) string {
	if k := tmpl.Stringify(v); k != "" {
		return k
	}
	return mock.NoKey
}

// ---- log -----------------------------------------------------------------

// log: {service, grep: regex}: lines the service logged since the scenario started.
// result: {count, lines}
func (ru *run) execLog(a map[string]any) (map[string]any, error) {
	svc := str(a, "service")
	path, ok := ru.r.Stack.LogFile(svc)
	if !ok {
		return nil, fmt.Errorf("no service or mock named %q", svc)
	}
	re, err := regexp.Compile(str(a, "grep"))
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if off := ru.logOffs[svc]; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	lines := []any{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		if re.MatchString(sc.Text()) {
			lines = append(lines, sc.Text())
		}
	}
	return map[string]any{"count": len(lines), "lines": lines}, nil
}

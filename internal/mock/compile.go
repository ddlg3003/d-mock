package mock

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Global variable names used inside Mockoon. Field "fault"/"faultn" are reserved.
//
//	<mock>|<field>|<key>   state field value for one key
//	<mock>|fault|<key>     active fault name
//	<mock>|faultn|<key>    remaining fault hits ("inf" = until cleared)
func VarName(mock, field, key string) string { return mock + "|" + field + "|" + key }

const NoKey = "_"

var placeholderRe = regexp.MustCompile(`\$\{(key|state\.[A-Za-z0-9_\-]+)\}`)

// Compile builds the Mockoon environment JSON for one mock. specDir resolves body_file paths.
func Compile(name string, port int, spec *Spec, specDir string) ([]byte, error) {
	var routes []map[string]any
	for _, r := range spec.Routes {
		cr, err := compileRoute(name, r, specDir)
		if err != nil {
			return nil, fmt.Errorf("mock %s route %s: %w", name, r.ID, err)
		}
		routes = append(routes, cr)
	}
	root := make([]map[string]any, 0, len(routes))
	for _, r := range routes {
		root = append(root, map[string]any{"type": "route", "uuid": r["uuid"]})
	}
	env := map[string]any{
		"uuid": uuidFor(name), "lastMigration": 33, "name": name, "endpointPrefix": "", "latency": 0,
		"port": port, "hostname": "", "folders": []any{}, "routes": routes, "rootChildren": root,
		"proxyMode": false, "proxyHost": "", "proxyRemovePrefix": false,
		"tlsOptions": map[string]any{"enabled": false, "type": "CERT", "pfxPath": "", "certPath": "", "keyPath": "", "caPath": "", "passphrase": ""},
		"cors":       true, "headers": []any{}, "proxyReqHeaders": []any{}, "proxyResHeaders": []any{}, "data": []any{}, "callbacks": []any{},
	}
	return json.MarshalIndent(env, "", " ")
}

// keyExpr is the Handlebars expression yielding the state key.
func keyExpr(r Route) string {
	if strings.TrimSpace(r.Key) == "" {
		return "'" + NoKey + "'"
	}
	return "(" + r.Key + ")"
}

func varExpr(mock, field string, r Route) string {
	return fmt.Sprintf("(getGlobalVar (concat '%s' %s))", escapeSQ(VarName(mock, field, "")), keyExpr(r))
}

// stateValue renders a state field raw ({{{ }}}: Mockoon's {{ }} HTML-escapes), falling back
// to the route's declared default when the key has no value yet.
func stateValue(mock, field string, r Route) string {
	v := varExpr(mock, field, r)
	return fmt.Sprintf("{{#if %s}}{{{getGlobalVar (concat '%s' %s)}}}{{else}}%s{{/if}}",
		v, escapeSQ(VarName(mock, field, "")), keyExpr(r), r.State[field])
}

func expandBody(mock, body string, r Route) (string, error) {
	var err error
	out := placeholderRe.ReplaceAllStringFunc(body, func(m string) string {
		ref := m[2 : len(m)-1]
		if ref == "key" {
			return "{{{" + unwrap(keyExpr(r)) + "}}}"
		}
		field := strings.TrimPrefix(ref, "state.")
		if _, ok := r.State[field]; !ok {
			err = fmt.Errorf("body uses ${state.%s} but the route declares no default for it under state", field)
		}
		return stateValue(mock, field, r)
	})
	return out, err
}

func compileRoute(mock string, r Route, specDir string) (map[string]any, error) {
	var responses []map[string]any
	baseBody, baseStatic, err := loadBody(r.Body, r.BodyFile, r.Encode, specDir)
	if err != nil {
		return nil, err
	}
	if !baseStatic {
		if baseBody, err = expandBody(mock, baseBody, r); err != nil {
			return nil, err
		}
	}

	// faults first: they win over variants and the default
	for _, fname := range sortedKeys(r.Faults) {
		f := r.Faults[fname]
		body, static := baseBody, baseStatic
		if f.Body != "" {
			static = false
			if body, err = expandBody(mock, f.Body, r); err != nil {
				return nil, err
			}
		}
		status := f.Status
		if status == 0 {
			status = statusOr(r.Status)
		}
		n := varExpr(mock, "faultn", r)
		decrement := fmt.Sprintf("{{#unless (eq %s 'inf')}}{{setGlobalVar (concat '%s' %s) (subtract %s 1)}}{{/unless}}",
			n, escapeSQ(VarName(mock, "faultn", "")), keyExpr(r), n)
		rules := []map[string]any{
			rule("{{"+unwrap(varExpr(mock, "fault", r))+"}}", fname, "equals"),
			rule("{{"+unwrap(n)+"}}", "^([1-9][0-9]*|inf)$", "regex"),
		}
		if static {
			// a pre-encoded body can't carry the counter template: the fault then lasts until cleared
			decrement = ""
		}
		responses = append(responses, response(mock, r.ID, "fault:"+fname, status, r.Headers, decrement+body, f.Latency.Milliseconds(), rules, false, static))
	}

	for i, v := range r.Variants {
		body, static, err := loadBody(v.Body, v.BodyFile, v.Encode, specDir)
		if err != nil {
			return nil, err
		}
		if !static {
			if body, err = expandBody(mock, body, r); err != nil {
				return nil, err
			}
		}
		var rules []map[string]any
		for _, field := range sortedKeys(v.When) {
			if _, ok := r.State[field]; !ok {
				return nil, fmt.Errorf("variant %d matches state.%s but the route declares no default for it", i, field)
			}
			// same default-aware expression as the body, so an unset key matches its default
			rules = append(rules, rule(stateValue(mock, field, r), "^"+regexp.QuoteMeta(v.When[field])+"$", "regex"))
		}
		headers := v.Headers
		if headers == nil {
			headers = r.Headers
		}
		status := v.Status
		if status == 0 {
			status = statusOr(r.Status)
		}
		responses = append(responses, response(mock, r.ID, fmt.Sprintf("variant:%d", i), status, headers, body, v.Latency.Milliseconds(), rules, false, static))
	}

	responses = append(responses, response(mock, r.ID, "default", statusOr(r.Status), r.Headers, baseBody, r.Latency.Milliseconds(), nil, true, baseStatic))

	return map[string]any{
		"uuid": uuidFor(mock + "/" + r.ID), "type": "http", "documentation": r.ID,
		"method": strings.ToLower(r.Method), "endpoint": strings.TrimPrefix(r.Path, "/"),
		"responses": responses, "responseMode": nil, "streamingMode": nil, "streamingInterval": 0,
	}, nil
}

// loadBody returns the body and whether it is static (encoded output, served with templating off).
func loadBody(body, file string, encode []string, dir string) (string, bool, error) {
	if file == "" {
		if len(encode) > 0 {
			return "", false, fmt.Errorf("encode needs body_file")
		}
		return body, false, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return "", false, err
	}
	if len(encode) == 0 {
		return string(b), false, nil
	}
	cmd := exec.Command(encode[0], encode[1:]...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(b)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("encode %v: %w: %s", encode, err, stderr.String())
	}
	return string(out), true, nil
}

func response(mock, routeID, label string, status int, headers map[string]string, body string, latencyMs int64, rules []map[string]any, def, static bool) map[string]any {
	hs := []map[string]string{}
	for _, k := range sortedKeys(headers) {
		hs = append(hs, map[string]string{"key": k, "value": headers[k]})
	}
	if rules == nil {
		rules = []map[string]any{}
	}
	return map[string]any{
		"uuid": uuidFor(mock + "/" + routeID + "/" + label), "body": body, "latency": latencyMs,
		"statusCode": status, "label": label, "headers": hs, "bodyType": "INLINE", "filePath": "",
		"databucketID": "", "sendFileAsBody": false, "rules": rules, "rulesOperator": "AND",
		"disableTemplating": static, "fallbackTo404": false, "default": def, "crudKey": "id", "callbacks": []any{},
	}
}

func rule(modifier, value, op string) map[string]any {
	return map[string]any{"target": "templating", "modifier": modifier, "value": value, "invert": false, "operator": op}
}

func statusOr(s int) int {
	if s == 0 {
		return 200
	}
	return s
}

func escapeSQ(s string) string { return strings.ReplaceAll(s, "'", `\'`) }

// uuidFor gives stable UUID-shaped ids so regenerated environments keep their identity.
func uuidFor(s string) string {
	h := sha1.Sum([]byte("d-mock:" + s))
	h[6] = (h[6] & 0x0f) | 0x50
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// unwrap drops one outer pair of parentheses from a Handlebars subexpression.
func unwrap(expr string) string {
	if strings.HasPrefix(expr, "(") && strings.HasSuffix(expr, ")") {
		return expr[1 : len(expr)-1]
	}
	return expr
}

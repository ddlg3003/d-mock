package mock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Admin talks to one running Mockoon environment's admin API.
type Admin struct {
	Name  string // mock name, the global-var prefix
	Base  string // http://localhost:<port>
	Token string
	HTTP  *http.Client
}

func NewAdmin(name string, port int, token string) *Admin {
	return &Admin{Name: name, Base: fmt.Sprintf("http://localhost:%d", port), Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (a *Admin) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.Base+"/mockoon-admin"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("mock %s admin %s %s: %w", a.Name, method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("mock %s admin %s %s: HTTP %d: %s", a.Name, method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (a *Admin) SetVar(key, value string) error {
	return a.do(http.MethodPost, "/global-vars", map[string]string{"key": key, "value": value}, nil)
}

func (a *Admin) GetVar(key string) (any, error) {
	var out struct {
		Value any `json:"value"`
	}
	err := a.do(http.MethodGet, "/global-vars/"+url.PathEscape(key), nil, &out)
	return out.Value, err
}

// SetState sets state fields for one key (use NoKey for routes without a key).
func (a *Admin) SetState(key string, fields map[string]string) error {
	for _, f := range sortedKeys(fields) {
		if f == "fault" || f == "faultn" {
			return fmt.Errorf("state field %q is reserved", f)
		}
		if err := a.SetVar(VarName(a.Name, f, key), fields[f]); err != nil {
			return err
		}
	}
	return nil
}

// SetFault turns a fault on for a key: times > 0 = that many hits, times == 0 = until cleared.
// name "" clears it.
func (a *Admin) SetFault(key, name string, times int) error {
	n := "inf"
	if times > 0 {
		n = strconv.Itoa(times)
	}
	if name == "" {
		n = "0"
	}
	if err := a.SetVar(VarName(a.Name, "faultn", key), n); err != nil {
		return err
	}
	return a.SetVar(VarName(a.Name, "fault", key), name)
}

// Reset clears all state, faults and recorded calls.
func (a *Admin) Reset() error {
	if err := a.do(http.MethodPost, "/global-vars/purge", nil, nil); err != nil {
		return err
	}
	return a.do(http.MethodPost, "/logs/purge", nil, nil)
}

// Call is one request Mockoon served.
type Call struct {
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Route       string            `json:"route"`
	RouteID     string            `json:"route_id,omitempty"`
	Query       string            `json:"query,omitempty"`
	Body        string            `json:"body,omitempty"`
	Status      int               `json:"status"`
	Response    string            `json:"response,omitempty"`
	TimestampMs int64             `json:"timestamp_ms"`
	Params      map[string]string `json:"params,omitempty"`
}

type rawTx struct {
	Request struct {
		Method  string `json:"method"`
		URLPath string `json:"urlPath"`
		Route   string `json:"route"`
		Query   string `json:"query"`
		Body    string `json:"body"`
		Params  []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"params"`
	} `json:"request"`
	Response struct {
		StatusCode int    `json:"statusCode"`
		Body       string `json:"body"`
	} `json:"response"`
	RouteUUID   string `json:"routeUUID"`
	TimestampMs int64  `json:"timestampMs"`
}

// Calls returns recorded requests, oldest first. spec maps route UUIDs back to route ids.
func (a *Admin) Calls(spec *Spec) ([]Call, error) {
	var txs []rawTx
	if err := a.do(http.MethodGet, "/logs", nil, &txs); err != nil {
		return nil, err
	}
	ids := map[string]string{}
	if spec != nil {
		for _, r := range spec.Routes {
			ids[uuidFor(a.Name+"/"+r.ID)] = r.ID
		}
	}
	calls := make([]Call, 0, len(txs))
	for _, t := range txs {
		if strings.HasPrefix(t.Request.URLPath, "/mockoon-admin") {
			continue // d-mock's own readiness probes
		}
		c := Call{
			Method: strings.ToUpper(t.Request.Method), Path: t.Request.URLPath, Route: t.Request.Route,
			RouteID: ids[t.RouteUUID], Query: t.Request.Query, Body: t.Request.Body,
			Status: t.Response.StatusCode, Response: t.Response.Body, TimestampMs: t.TimestampMs,
		}
		if len(t.Request.Params) > 0 {
			c.Params = map[string]string{}
			for _, p := range t.Request.Params {
				c.Params[p.Name] = p.Value
			}
		}
		calls = append(calls, c)
	}
	// Mockoon returns newest first in some versions: normalise to oldest first
	if len(calls) > 1 && calls[0].TimestampMs > calls[len(calls)-1].TimestampMs {
		for i, j := 0, len(calls)-1; i < j; i, j = i+1, j-1 {
			calls[i], calls[j] = calls[j], calls[i]
		}
	}
	return calls, nil
}

// Filter keeps calls to routeID (if set) whose path, query or body contains key (if set).
func Filter(calls []Call, routeID, key string) []Call {
	var out []Call
	for _, c := range calls {
		if routeID != "" && c.RouteID != routeID {
			continue
		}
		if key != "" && !strings.Contains(c.Path, key) && !strings.Contains(c.Query, key) && !strings.Contains(c.Body, key) {
			continue
		}
		out = append(out, c)
	}
	return out
}

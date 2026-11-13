package scenario

import "testing"

func TestParseK6Summary(t *testing.T) {
	raw := `{"metrics":{
	  "http_req_duration":{"avg":12.5,"min":1,"med":10,"max":90,"p(90)":30,"p(95)":42.5,"p(99.9)":88,"thresholds":{"p(95)<500":false}},
	  "http_req_failed":{"passes":2,"fails":98,"value":0.02},
	  "http_reqs":{"count":100,"rate":9.8}},
	 "root_group":{"checks":{"status is 200":{"name":"status is 200","passes":98,"fails":2}}}}`
	res, err := parseK6Summary([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if f := Check(res, map[string]any{
		"metrics.http_req_duration.p95":        map[string]any{"lt": 50},
		"metrics.http_req_duration.p99_9":      88,
		"metrics.http_req_failed.rate":         map[string]any{"lte": 0.02},
		"metrics.http_reqs.count":              100,
		"checks.status is 200.fails":           2,
		"metrics.http_req_duration.thresholds": map[string]any{"exists": false},
	}); len(f) != 0 {
		t.Fatalf("failures: %v", f)
	}
}

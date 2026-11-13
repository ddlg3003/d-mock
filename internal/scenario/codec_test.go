package scenario

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPEncodeDecode: the request body is filled in, then encoded by a command; the
// response is decoded by a command and json comes from the decoded text, body stays raw.
func TestHTTPEncodeDecode(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Write([]byte(`{"STATUS":"APPROVED","ID":"DM1"}`)) // "encrypted" = upper case
	}))
	defer srv.Close()
	res, err := execHTTP(context.Background(), map[string]any{
		"method": "POST", "url": srv.URL,
		"json":   map[string]any{"id": "dm1"},
		"encode": []any{"tr", "a-z", "A-Z"},
		"decode": []any{"tr", "A-Z", "a-z"},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"ID":"DM1"}` {
		t.Errorf("sent %q, want the encoded body", got)
	}
	if res["body"] != `{"STATUS":"APPROVED","ID":"DM1"}` || res["decoded"] != `{"status":"approved","id":"dm1"}` {
		t.Errorf("body %q decoded %q", res["body"], res["decoded"])
	}
	if fails := Check(res, map[string]any{"json.status": "approved", "json.id": "dm1"}); len(fails) > 0 {
		t.Error(fails)
	}
	// a shell line is refused: a payload must never pass through a shell parser
	if _, err := execHTTP(context.Background(), map[string]any{"url": srv.URL, "encode": "tr a-z A-Z"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "list") {
		t.Errorf("encode as a string: %v", err)
	}
	// a failing decoder is an error that says so
	if _, err := execHTTP(context.Background(), map[string]any{"url": srv.URL, "decode": []any{"false"}}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("failing decode: %v", err)
	}
}

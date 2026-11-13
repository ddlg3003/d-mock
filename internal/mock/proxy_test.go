package mock

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyCodec(t *testing.T) {
	var seen, seenType, adminSeen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/mockoon-admin") {
			adminSeen = string(b)
			w.Write([]byte(`{"admin":"plain"}`))
			return
		}
		seen, seenType = string(b), r.Header.Get("Content-Type")
		w.Header().Set("X-From", "mockoon")
		w.WriteHeader(201)
		w.Write([]byte(`{"ref":"r1","status":"ok"}`))
	}))
	defer upstream.Close()
	spec := &Spec{
		Codec: &Codec{Decode: []string{"tr", "A-Z", "a-z"}, Encode: []string{"tr", "a-z", "A-Z"}},
		Routes: []Route{
			{ID: "inquiry", Method: "POST", Path: "orders/:id/inquiry"},
			{ID: "plain", Method: "GET", Path: "health", Codec: &Codec{}},
		},
	}
	p := &Proxy{Spec: spec, Dir: t.TempDir(), Upstream: upstream.URL}
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/orders/7/inquiry", "text/plain", strings.NewReader(`{"REF":"R1"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if seen != `{"ref":"r1"}` || seenType != "application/json" {
		t.Errorf("mockoon saw %q (%s), want the decoded JSON", seen, seenType)
	}
	if resp.StatusCode != 201 || string(b) != `{"REF":"R1","STATUS":"OK"}` || resp.Header.Get("X-From") != "mockoon" {
		t.Errorf("client got %d %q %v, want status/headers kept and the body encoded", resp.StatusCode, b, resp.Header)
	}

	// a route with `codec: {}` and the admin API pass through untouched
	resp, _ = http.Get(srv.URL + "/health")
	b, _ = io.ReadAll(resp.Body)
	if string(b) != `{"ref":"r1","status":"ok"}` {
		t.Errorf("plain route was encoded: %q", b)
	}
	resp, _ = http.Post(srv.URL+"/mockoon-admin/state", "application/json", strings.NewReader(`{"K":"V"}`))
	b, _ = io.ReadAll(resp.Body)
	if adminSeen != `{"K":"V"}` || string(b) != `{"admin":"plain"}` {
		t.Errorf("admin API changed: sent %q got %q", adminSeen, b)
	}

	// a body the decoder rejects never reaches Mockoon
	p.Spec.Codec.Decode = []string{"false"}
	seen = ""
	resp, _ = http.Post(srv.URL+"/orders/7/inquiry", "text/plain", strings.NewReader("garbage"))
	if resp.StatusCode != 400 || seen != "" {
		t.Errorf("decode failure: status %d, mockoon saw %q", resp.StatusCode, seen)
	}
}

func TestMatchPath(t *testing.T) {
	s := &Spec{Routes: []Route{
		{ID: "a", Method: "GET", Path: "orders/:id"},
		{ID: "b", Method: "POST", Path: "files/*"},
	}}
	cases := map[string]string{"GET /orders/1": "a", "GET /orders/1/x": "", "POST /orders/1": "", "POST /files/a/b": "b"}
	for in, want := range cases {
		m, p, _ := strings.Cut(in, " ")
		got := ""
		if r := s.Match(m, p); r != nil {
			got = r.ID
		}
		if got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}

func TestSpecRefusesTwoEncodes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.yaml")
	os.WriteFile(p, []byte(`codec: { encode: [cat] }
routes:
  - { id: r, method: GET, path: x, body_file: f.json, encode: [cat] }
`), 0o644)
	if _, err := LoadSpec(p); err == nil || !strings.Contains(err.Error(), "keep one") {
		t.Errorf("codec encode + static encode: %v", err)
	}
}

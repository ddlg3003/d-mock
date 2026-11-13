package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/ddlg3003/d-mock/internal/codec"
)

// MaxBody bounds a request or response body the codec proxy reads.
const MaxBody = 16 << 20

// Proxy sits on a mock's port in front of Mockoon when the spec has a codec: it decodes a
// request body before Mockoon sees it and encodes Mockoon's (templated) response body on
// the way back, once per request. The admin API passes through untouched.
type Proxy struct {
	Spec     *Spec
	Dir      string // the spec's folder: codec commands run from here
	Upstream string // Mockoon, e.g. http://127.0.0.1:41234
	Client   *http.Client
}

// Serve listens on addr until ctx ends.
func (p *Proxy) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: p}
	go func() { <-ctx.Done(); srv.Close() }()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("d-mock codec proxy on %s → %s", addr, p.Upstream)
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var c *Codec
	route := ""
	if !strings.HasPrefix(r.URL.Path, "/mockoon-admin") {
		if rt := p.Spec.Match(r.Method, r.URL.Path); rt != nil {
			c, route = p.Spec.RouteCodec(rt), rt.ID
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		http.Error(w, "d-mock codec: request body unreadable or over 16 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	header := r.Header.Clone()
	header.Del("Accept-Encoding") // a compressed response can't be re-encoded
	if c != nil && len(c.Decode) > 0 && len(body) > 0 {
		plain, err := codec.Run(r.Context(), c.Decode, p.Dir, body)
		if err != nil {
			log.Printf("codec %s %s (route %s): decode failed: %v", r.Method, r.URL.Path, route, err)
			http.Error(w, "d-mock codec: request decode failed (see the mock log)", http.StatusBadRequest)
			return
		}
		log.Printf("codec %s %s (route %s): decoded request %d → %d bytes", r.Method, r.URL.Path, route, len(body), len(plain))
		// Mockoon reads body fields (key: bodyRaw 'x') by content type
		if json.Valid(plain) {
			header.Set("X-Dmock-Encoded-Content-Type", r.Header.Get("Content-Type"))
			header.Set("Content-Type", "application/json")
		}
		body = plain
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, p.Upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	up.Header = header
	up.Host = r.Host
	resp, err := p.client().Do(up)
	if err != nil {
		if c != nil || route != "" { // admin calls fail here while Mockoon boots: not news
			log.Printf("codec %s %s: mockoon: %v", r.Method, r.URL.Path, err)
		}
		http.Error(w, "d-mock codec: mockoon unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil || len(out) > MaxBody {
		http.Error(w, "d-mock codec: response unreadable or over 16 MiB", http.StatusBadGateway)
		return
	}
	if c != nil && len(c.Encode) > 0 && len(out) > 0 {
		enc, err := codec.Run(r.Context(), c.Encode, p.Dir, out)
		if err != nil {
			log.Printf("codec %s %s (route %s): encode failed: %v", r.Method, r.URL.Path, route, err)
			http.Error(w, "d-mock codec: response encode failed (see the mock log)", http.StatusBadGateway)
			return
		}
		log.Printf("codec %s %s (route %s): encoded response %d → %d bytes", r.Method, r.URL.Path, route, len(out), len(enc))
		out = enc
	}
	for k, vs := range resp.Header {
		if k == "Content-Length" || k == "Content-Encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

// Match returns the route a request goes to, by method and Mockoon path syntax
// (`orders/:id` matches one segment, `*` matches the rest), or nil.
func (s *Spec) Match(method, path string) *Route {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := range s.Routes {
		r := &s.Routes[i]
		if !strings.EqualFold(r.Method, method) && !strings.EqualFold(r.Method, "all") {
			continue
		}
		if matchPath(strings.Split(strings.Trim(r.Path, "/"), "/"), segs) {
			return r
		}
	}
	return nil
}

func matchPath(pat, segs []string) bool {
	for i, p := range pat {
		if p == "*" {
			return true
		}
		if i >= len(segs) {
			return false
		}
		if !strings.HasPrefix(p, ":") && p != segs[i] {
			return false
		}
	}
	return len(pat) == len(segs)
}

// String is for logs.
func (c *Codec) String() string {
	return fmt.Sprintf("decode=%v encode=%v", c.Decode, c.Encode)
}

// passthrough: no timeout (a latency fault may hold a response longer than any default) and
// redirects go back to the caller as they are
var passthrough = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (p *Proxy) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return passthrough
}

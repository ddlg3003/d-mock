// Toy "orders" service for the d-mock example. It has the shape d-mock was built to test:
// an order waits for a partner decision that arrives either by callback (signal path) or by
// polling the partner API (poll path), and is cancelled at the partner when neither comes
// in time (timeout path). State lives in Redis.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen          string        `yaml:"listen"`
	RedisAddr       string        `yaml:"redis_addr"`
	PartnerURL      string        `yaml:"partner_url"`
	PollDelay       time.Duration `yaml:"poll_delay"`
	PollEvery       time.Duration `yaml:"poll_every"`
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`
}

type Order struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // WAITING | APPROVED | REJECTED | CANCELLED
	DecidedBy string `json:"decided_by,omitempty"`
}

type server struct {
	cfg     Config
	redis   *redisClient
	mu      sync.Mutex
	signals map[string]chan struct{}
	http    *http.Client
}

func main() {
	path := os.Getenv("ORDERS_CONFIG")
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read config %q: %v", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		log.Fatal(err)
	}
	s := &server{cfg: cfg, redis: &redisClient{addr: cfg.RedisAddr}, signals: map[string]chan struct{}{}, http: &http.Client{Timeout: 3 * time.Second}}
	if _, err := s.redis.do("PING"); err != nil {
		log.Fatalf("redis %s: %v", cfg.RedisAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /orders", s.create)
	mux.HandleFunc("GET /orders/{id}", s.get)
	mux.HandleFunc("POST /callback", s.callback)
	log.Printf("orders listening on %s (partner %s)", cfg.Listen, cfg.PartnerURL)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	o := Order{ID: in.ID, Status: "WAITING"}
	if err := s.save(o); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sig := make(chan struct{}, 1)
	s.mu.Lock()
	s.signals[o.ID] = sig
	s.mu.Unlock()
	go s.awaitDecision(o.ID, sig)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(o)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	o, err := s.load(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(o)
}

// callback is the partner pushing its decision.
func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	var in struct{ ID, Status string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	applied, err := s.decide(in.ID, in.Status, "callback")
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("callback order=%s status=%s applied=%v", in.ID, in.Status, applied)
	if applied {
		s.mu.Lock()
		if sig, ok := s.signals[in.ID]; ok {
			sig <- struct{}{}
		}
		s.mu.Unlock()
	}
	json.NewEncoder(w).Encode(map[string]any{"applied": applied})
}

// awaitDecision: signal first; after poll_delay poll the partner until approval_timeout.
func (s *server) awaitDecision(id string, sig chan struct{}) {
	defer func() {
		s.mu.Lock()
		delete(s.signals, id)
		s.mu.Unlock()
	}()
	deadline := time.Now().Add(s.cfg.ApprovalTimeout)
	select {
	case <-sig:
		log.Printf("order=%s decided by signal, no poll", id)
		return
	case <-time.After(s.cfg.PollDelay):
	}
	for time.Now().Before(deadline) {
		select {
		case <-sig:
			return
		default:
		}
		status, err := s.pollPartner(id)
		if err != nil {
			log.Printf("order=%s partner error: %v", id, err)
		} else if status == "APPROVED" || status == "REJECTED" {
			applied, _ := s.decide(id, status, "poll")
			log.Printf("order=%s polled status=%s applied=%v", id, status, applied)
			return
		} else {
			log.Printf("order=%s partner still %s", id, status)
		}
		time.Sleep(s.cfg.PollEvery)
	}
	if applied, _ := s.decide(id, "CANCELLED", "timeout"); applied {
		log.Printf("order=%s approval timeout, cancelling at partner", id)
		resp, err := s.http.Post(s.cfg.PartnerURL+"/orders/"+id+"/cancel", "application/json", nil)
		if err != nil {
			log.Printf("order=%s partner cancel failed: %v", id, err)
			return
		}
		resp.Body.Close()
	}
}

func (s *server) pollPartner(id string) (string, error) {
	resp, err := s.http.Get(s.cfg.PartnerURL + "/orders/" + id)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct{ Status string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// decide moves a WAITING order to a final status; a decided order is never overwritten.
func (s *server) decide(id, status, by string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.load(id)
	if err != nil {
		return false, err
	}
	if o.Status != "WAITING" {
		return false, nil
	}
	o.Status, o.DecidedBy = status, by
	return true, s.save(o)
}

func (s *server) save(o Order) error {
	b, _ := json.Marshal(o)
	_, err := s.redis.do("SET", "order:"+o.ID, string(b))
	return err
}

func (s *server) load(id string) (Order, error) {
	v, err := s.redis.do("GET", "order:"+id)
	if err != nil {
		return Order{}, err
	}
	if v == "" {
		return Order{}, fmt.Errorf("order %s not found", id)
	}
	var o Order
	return o, json.Unmarshal([]byte(v), &o)
}

// redisClient is just enough RESP for PING/GET/SET, to keep the example dependency-free.
type redisClient struct{ addr string }

func (c *redisClient) do(args ...string) (string, error) {
	conn, err := net.DialTimeout("tcp", c.addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return "", err
	}
	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return "", errors.New(line[1:])
	case '$':
		var n int
		fmt.Sscan(line[1:], &n)
		if n < 0 {
			return "", nil
		}
		buf := make([]byte, n+2)
		if _, err := ioReadFull(rd, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	}
	return line, nil
}

func ioReadFull(rd *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := rd.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

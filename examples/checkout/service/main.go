// Toy "checkout" service for the d-mock checkout example. It stands in for your own service:
// it stores orders in MySQL and charges them through an external payment provider, retrying
// a failed or timed-out charge with the same idempotency key.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/segmentio/kafka-go"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen          string        `yaml:"listen"`
	DBDSN           string        `yaml:"db_dsn"`
	PaymentsURL     string        `yaml:"payments_url"`
	PaymentsTimeout time.Duration `yaml:"payments_timeout"`
	ChargeAttempts  int           `yaml:"charge_attempts"`
	KafkaBrokers    []string      `yaml:"kafka_brokers"` // optional: publish order events
	EventsTopic     string        `yaml:"events_topic"`
}

type server struct {
	cfg  Config
	db   *sql.DB
	http *http.Client
	kw   *kafka.Writer // nil when kafka_brokers is not set
}

const schema = "CREATE TABLE IF NOT EXISTS orders (id VARCHAR(64) PRIMARY KEY, amount BIGINT NOT NULL, status VARCHAR(16) NOT NULL)"

func main() {
	migrate := flag.String("migrate", "", "create the schema in this DSN and exit (stands in for your migration tool)")
	flag.Parse()
	if *migrate != "" {
		db, err := sql.Open("mysql", *migrate)
		if err == nil {
			_, err = db.Exec(schema)
		}
		if err != nil {
			log.Fatalf("migrate: %v", err)
		}
		log.Print("migrate: orders table ready")
		return
	}
	b, err := os.ReadFile(os.Getenv("CHECKOUT_CONFIG"))
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.DBDSN)
	if err != nil {
		log.Fatal(err)
	}
	// size the pool: the default keeps only 2 idle connections, so under load almost every
	// query dials MySQL again (the load example's "lost charges" came from exactly that)
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(20)
	s := &server{cfg: cfg, db: db, http: &http.Client{Timeout: cfg.PaymentsTimeout}}
	if len(cfg.KafkaBrokers) > 0 {
		s.kw = &kafka.Writer{Addr: kafka.TCP(cfg.KafkaBrokers...), Topic: cfg.EventsTopic, AllowAutoTopicCreation: true, BatchTimeout: 10 * time.Millisecond}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /orders", s.createOrder)
	mux.HandleFunc("POST /checkout", s.checkout)
	log.Printf("checkout listening on %s (payments %s)", cfg.Listen, cfg.PaymentsURL)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

func (s *server) createOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Amount int64  `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
		http.Error(w, "id and amount required", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec("INSERT INTO orders (id, amount, status) VALUES (?, ?, 'NEW')", in.ID, in.Amount); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// checkout claims the order (NEW → CHARGING) and charges it in the background. The claim is
// one conditional UPDATE, so of two checkouts arriving together exactly one wins; the other
// gets 409. A DB that is down is a 503 (try again), not a 404.
func (s *server) checkout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID string `json:"order_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.OrderID == "" {
		http.Error(w, "order_id required", http.StatusBadRequest)
		return
	}
	var amount int64
	err := s.db.QueryRow("SELECT amount FROM orders WHERE id = ?", in.OrderID).Scan(&amount)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("order=%s checkout: db: %v", in.OrderID, err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	res, err := s.db.Exec("UPDATE orders SET status = 'CHARGING' WHERE id = ? AND status = 'NEW'", in.OrderID)
	if err != nil {
		log.Printf("order=%s checkout: db: %v", in.OrderID, err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "checkout already in progress or done", http.StatusConflict)
		return
	}
	go s.charge(in.OrderID, amount)
	w.WriteHeader(http.StatusAccepted)
}

// charge calls the provider until it answers, retrying timeouts and 5xx with the SAME
// idempotency key, so the provider can tell a retry from a second charge.
func (s *server) charge(id string, amount int64) {
	body, _ := json.Marshal(map[string]any{"order_id": id, "amount": amount, "idempotency_key": "pay-" + id})
	for attempt := 1; attempt <= s.cfg.ChargeAttempts; attempt++ {
		status, err := s.post(body)
		switch {
		case err != nil:
			log.Printf("order=%s charge attempt %d failed: %v", id, attempt, err)
		case status == "SUCCEEDED":
			s.setStatus(id, "PAID")
			log.Printf("order=%s paid after %d attempt(s)", id, attempt)
			return
		default: // a business answer such as DECLINED: retrying would not change it
			s.setStatus(id, "FAILED")
			log.Printf("order=%s charge %s, not retrying", id, status)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.setStatus(id, "FAILED")
	log.Printf("order=%s gave up after %d attempts", id, s.cfg.ChargeAttempts)
}

func (s *server) post(body []byte) (string, error) {
	resp, err := s.http.Post(s.cfg.PaymentsURL+"/charges", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Status, nil
}

func (s *server) setStatus(id, status string) {
	if _, err := s.db.Exec("UPDATE orders SET status = ? WHERE id = ?", status, id); err != nil {
		log.Printf("order=%s set status %s: %v", id, status, err)
		return
	}
	s.publish(id, status)
}

// publish tells the rest of the system the order's outcome (key = order id).
func (s *server) publish(id, status string) {
	if s.kw == nil {
		return
	}
	v, _ := json.Marshal(map[string]string{"order_id": id, "status": status})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.kw.WriteMessages(ctx, kafka.Message{Key: []byte(id), Value: v}); err != nil {
		log.Printf("order=%s publish %s: %v", id, status, err)
	}
}

// Toy "app" service for the d-mock encrypted example. It stands in for your own service:
// it asks a third-party API about a reference, where every request and response body is
// encrypted, and it checks that the decrypted answer is about the reference it asked for.
// It also accepts encrypted callbacks from that API.
//
// The encryption is `openssl enc -aes-256-cbc -pbkdf2 -iter 10000 -md sha256 -a -A`
// (base64 of "Salted__" + salt + ciphertext), so the d-mock codec can use plain openssl.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen     string        `yaml:"listen"`
	APIURL     string        `yaml:"api_url"`
	KeyFile    string        `yaml:"key_file"`
	APITimeout time.Duration `yaml:"api_timeout"`
}

type server struct {
	cfg    Config
	pass   string
	http   *http.Client
	mu     sync.Mutex
	status map[string]string
}

func main() {
	b, err := os.ReadFile(os.Getenv("APP_CONFIG"))
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		log.Fatal(err)
	}
	key, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{cfg: cfg, pass: strings.TrimSpace(string(key)), http: &http.Client{Timeout: cfg.APITimeout}, status: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /requests", s.create)
	mux.HandleFunc("GET /requests/{ref}", s.get)
	mux.HandleFunc("POST /callbacks", s.callback)
	log.Printf("app listening on %s (api %s)", cfg.Listen, cfg.APIURL)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

type answer struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
}

// create asks the API about a reference and keeps its decision.
func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Ref == "" {
		http.Error(w, "ref required", http.StatusBadRequest)
		return
	}
	plain, _ := json.Marshal(map[string]string{"ref": in.Ref})
	enc, err := encrypt(plain, s.pass)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := s.http.Post(s.cfg.APIURL+"/inquiry", "text/plain", strings.NewReader(enc))
	if err != nil {
		s.fail(w, in.Ref, fmt.Sprintf("api: %v", err))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		s.fail(w, in.Ref, fmt.Sprintf("api: HTTP %d", resp.StatusCode))
		return
	}
	dec, err := decrypt(string(body), s.pass)
	if err != nil {
		s.fail(w, in.Ref, "api response does not decrypt: "+err.Error())
		return
	}
	var a answer
	if err := json.Unmarshal(dec, &a); err != nil {
		s.fail(w, in.Ref, "api response is not JSON")
		return
	}
	// the answer must be about the reference we asked for: never act on someone else's
	if a.Ref != in.Ref {
		s.fail(w, in.Ref, fmt.Sprintf("response ref mismatch: got %q", a.Ref))
		return
	}
	s.set(in.Ref, a.Status)
	log.Printf("ref=%s status=%s", in.Ref, a.Status)
	json.NewEncoder(w).Encode(answer{Ref: in.Ref, Status: a.Status})
}

func (s *server) fail(w http.ResponseWriter, ref, why string) {
	s.set(ref, "ERROR")
	log.Printf("ref=%s error: %s", ref, why)
	w.WriteHeader(http.StatusBadGateway)
	json.NewEncoder(w).Encode(map[string]string{"ref": ref, "status": "ERROR", "error": why})
}

// callback takes an encrypted decision pushed by the API later.
func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	dec, err := decrypt(string(body), s.pass)
	if err != nil {
		log.Printf("callback rejected: does not decrypt: %v", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	var a answer
	if err := json.Unmarshal(dec, &a); err != nil || a.Ref == "" {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, known := s.status[a.Ref]
	if known {
		s.status[a.Ref] = a.Status
	}
	s.mu.Unlock()
	if !known {
		http.Error(w, "unknown ref", http.StatusNotFound)
		return
	}
	log.Printf("ref=%s status=%s (callback)", a.Ref, a.Status)
	w.WriteHeader(http.StatusOK)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	s.mu.Lock()
	st, ok := s.status[ref]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(answer{Ref: ref, Status: st})
}

func (s *server) set(ref, status string) {
	s.mu.Lock()
	s.status[ref] = status
	s.mu.Unlock()
}

// ---- openssl enc -aes-256-cbc -pbkdf2 -iter 10000 -md sha256 -a -A ----

func deriveKey(pass string, salt []byte) (key, iv []byte, err error) {
	kiv, err := pbkdf2.Key(sha256.New, pass, salt, 10000, 48)
	if err != nil {
		return nil, nil, err
	}
	return kiv[:32], kiv[32:], nil
}

func encrypt(plain []byte, pass string) (string, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, iv, err := deriveKey(pass, salt)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(key)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	return base64.StdEncoding.EncodeToString(append(append([]byte("Salted__"), salt...), data...)), nil
}

func decrypt(text, pass string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, err
	}
	if len(raw) < 16+aes.BlockSize || string(raw[:8]) != "Salted__" || (len(raw)-16)%aes.BlockSize != 0 {
		return nil, errors.New("not an openssl salted ciphertext")
	}
	key, iv, err := deriveKey(pass, raw[8:16])
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	data := raw[16:]
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return nil, errors.New("bad padding (wrong key?)")
	}
	return data[:len(data)-pad], nil
}

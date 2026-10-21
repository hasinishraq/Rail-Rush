// Command fakesms is the local SMS gateway stand-in (§25): it accepts
// POST /send {"to","text"}, prints every message (OTPs included, so a developer
// can read them) and serves GET /last?phone= for tests.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type message struct {
	To   string    `json:"to"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

type store struct {
	mu   sync.Mutex
	byTo map[string][]message
}

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8091"
	}
	s := &store{byTo: map[string][]message{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", s.send)
	mux.HandleFunc("GET /last", s.last)
	mux.HandleFunc("GET /messages", s.messages)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	log.Printf("fake SMS gateway on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *store) send(w http.ResponseWriter, r *http.Request) {
	var m message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&m); err != nil || m.To == "" || m.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "need to and text"})
		return
	}
	m.At = time.Now().UTC()
	s.mu.Lock()
	list := append(s.byTo[m.To], m)
	if len(list) > 50 {
		list = list[len(list)-50:]
	}
	s.byTo[m.To] = list
	s.mu.Unlock()
	log.Printf("SMS to %s: %s", m.To, m.Text)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *store) last(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	list := s.byTo[r.URL.Query().Get("phone")]
	s.mu.Unlock()
	if len(list) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no message"})
		return
	}
	writeJSON(w, http.StatusOK, list[len(list)-1])
}

func (s *store) messages(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	list := append([]message{}, s.byTo[r.URL.Query().Get("phone")]...)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

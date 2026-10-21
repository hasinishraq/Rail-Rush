package api

import (
	"testing"

	"railway/internal/platform/config"
)

func TestReturnURLAllowed(t *testing.T) {
	s := &Server{cfg: config.Config{ReturnURLs: []string{"http://localhost", "https://railrush.example/app"}}}
	for u, want := range map[string]bool{
		"http://localhost/pay/return":           true,
		"http://localhost:8088/pay/return":      true, // no port in the entry: any port
		"https://railrush.example/app/return":   true,
		"http://localhost.evil.com/pay/return":  false, // passed the old prefix check
		"http://localhost@evil.com/pay/return":  false,
		"https://localhost/pay/return":          false, // scheme must match
		"https://railrush.example/other":        false, // outside the path prefix
		"https://railrush.example.evil.com/app": false,
		"javascript:alert(1)":                   false,
		"/pay/return":                           false,
		"":                                      false,
	} {
		if got := s.returnURLAllowed(u); got != want {
			t.Errorf("returnURLAllowed(%q) = %v, want %v", u, got, want)
		}
	}
}

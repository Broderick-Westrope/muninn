package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostCheck(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := hostCheck("127.0.0.1", next)

	do := func(host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	allowed := []string{
		"localhost:7576",
		"localhost",
		"127.0.0.1",
		"127.0.0.1:7576",
		"[::1]:7576",
		"[::1]",
		"127.0.0.2:80",
	}
	for _, host := range allowed {
		rec := do(host)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: status = %d, want 200", host, rec.Code)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("Host %q: X-Content-Type-Options = %q, want nosniff", host, got)
		}
	}

	rejected := []string{
		"evil.com",
		"evil.com:7576",
		"192.168.1.5",
		"192.168.1.5:7576",
		"10.0.0.1:80",
		"[2001:db8::1]:7576",
		"",
	}
	for _, host := range rejected {
		rec := do(host)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status = %d, want 403", host, rec.Code)
		}
	}

	// The exact bound host is allowed even when it is not loopback
	// (--unsafe-listen serving a named host).
	named := hostCheck("myhost.internal", next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "myhost.internal:7576"
	rec := httptest.NewRecorder()
	named.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("bound host: status = %d, want 200", rec.Code)
	}
}

func TestResolveListenAddr(t *testing.T) {
	// IP literals pass through unchanged.
	for _, addr := range []string{"127.0.0.1:7576", "127.0.0.2:80", "[::1]:7576"} {
		got, err := ResolveListenAddr(addr)
		if err != nil {
			t.Errorf("ResolveListenAddr(%q) error = %v, want nil", addr, err)
			continue
		}
		if got != addr {
			t.Errorf("ResolveListenAddr(%q) = %q, want unchanged", addr, got)
		}
	}

	// Hostnames resolve to a concrete loopback IP literal (no later
	// re-resolution by Listen).
	got, err := ResolveListenAddr("localhost:0")
	if err != nil {
		t.Fatalf("ResolveListenAddr(localhost:0) error = %v", err)
	}
	if got != "127.0.0.1:0" && got != "[::1]:0" {
		t.Errorf("ResolveListenAddr(localhost:0) = %q, want a loopback IP literal", got)
	}

	refused := []string{
		"0.0.0.0:7576",
		"[::]:7576",
		"192.168.1.5:7576",
		"10.0.0.1:80",
		":7576",
		"127.0.0.1", // no port
	}
	for _, addr := range refused {
		if _, err := ResolveListenAddr(addr); err == nil {
			t.Errorf("ResolveListenAddr(%q) = nil error, want error", addr)
		}
	}
}

package authplugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyTurnstileHTTP(t *testing.T) {
	var gotSecret, gotResponse, gotIP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		gotSecret = r.Form.Get("secret")
		gotResponse = r.Form.Get("response")
		gotIP = r.Form.Get("remoteip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"hostname":"app.example"}`))
	}))
	defer srv.Close()

	cfg := Config{TurnstileSecret: "s3cret", TurnstileVerifyURL: srv.URL}
	hostname, err := verifyTurnstileHTTP(context.Background(), cfg, "token-1", "203.0.113.7")
	if err != nil {
		t.Fatalf("verifyTurnstileHTTP: %v", err)
	}
	if hostname != "app.example" {
		t.Fatalf("hostname = %q, want app.example", hostname)
	}
	if gotSecret != "s3cret" || gotResponse != "token-1" || gotIP != "203.0.113.7" {
		t.Fatalf("form values secret=%q response=%q remoteip=%q", gotSecret, gotResponse, gotIP)
	}
}

func TestVerifyTurnstileHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	defer srv.Close()

	cfg := Config{TurnstileSecret: "s3cret", TurnstileVerifyURL: srv.URL}
	if _, err := verifyTurnstileHTTP(context.Background(), cfg, "bad", ""); err == nil {
		t.Fatal("expected verification failure")
	}
}

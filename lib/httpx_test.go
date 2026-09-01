package rota

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDoCapsResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", maxBody+5000)))
	}))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	raw, err := do(req)
	if err != nil || len(raw) != maxBody {
		t.Fatalf("len=%d err=%v", len(raw), err)
	}
}

func TestPostFormDecodesErrorBodyOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type %q", ct)
		}
		if r.Header.Get("User-Agent") != UserAgent || r.Header.Get("X-Custom") != "1" {
			t.Errorf("headers: %v", r.Header)
		}
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("form: %v", r.Form)
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"gone"}`))
	}))
	defer srv.Close()
	var out struct {
		Error string `json:"error"`
	}
	err := postForm(context.Background(), srv.URL, url.Values{"grant_type": {"refresh_token"}}, &out, map[string]string{"X-Custom": "1"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 || out.Error != "invalid_grant" {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if !strings.Contains(err.Error(), "http 400") {
		t.Fatalf("message: %v", err)
	}
}

func TestPostJSONAndGetJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post":
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content-type %q", r.Header.Get("Content-Type"))
			}
			w.Write([]byte(`{"ok":true}`))
		case "/get":
			if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Accept") != "application/json" {
				t.Errorf("headers: %v", r.Header)
			}
			w.Write([]byte(`{"ok":true}`))
		case "/fail":
			w.WriteHeader(500)
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := postJSON(context.Background(), srv.URL+"/post", map[string]string{"a": "b"}, &out, nil); err != nil || !out.OK {
		t.Fatalf("post: %v %+v", err, out)
	}
	out.OK = false
	if err := getJSON(context.Background(), srv.URL+"/get", "tok", &out, nil); err != nil || !out.OK {
		t.Fatalf("get: %v %+v", err, out)
	}
	out.OK = false
	err := postJSON(context.Background(), srv.URL+"/fail", nil, &out, nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 500 || !out.OK {
		t.Fatalf("fail: err=%v out=%+v (status must surface, body still decoded)", err, out)
	}
}

func TestPKCEChallengeIsS256OfVerifier(t *testing.T) {
	v, c := pkce()
	sum := sha256.Sum256([]byte(v))
	if c != base64.RawURLEncoding.EncodeToString(sum[:]) || len(v) < 43 {
		t.Fatalf("v=%q c=%q", v, c)
	}
}

func TestRandomHandles(t *testing.T) {
	id := randID()
	if len(id) != 6 || strings.Trim(id, "0123456789abcdef") != "" || id == randID() {
		t.Fatalf("id=%q", id)
	}
	if truncate("abcdef", 3) != "abc..." || truncate("ab", 3) != "ab" {
		t.Fatal("truncate")
	}
}

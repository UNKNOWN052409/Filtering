package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jsonNewDecoder(r io.Reader) *json.Decoder {
	return json.NewDecoder(r)
}

// ───────────────── stub auth server (mirrors validator.py test flows) ─────────────────

func newStubServer(t *testing.T, redirectMode bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// login: good creds -> Set-Cookie sid=good; bad -> 401
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := jsonDecode(r, &body); err != nil || body["login"] != "good" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Set-Cookie", "sid=good; Path=/; HttpOnly")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	})

	// protected endpoint; redirect mode bounces dead cookies to /login (302)
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "sid=good") {
			if redirectMode {
				http.Redirect(w, r, "/login", 302)
				return
			}
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":{"summary":{"userCountry":"IN","subPlan":"PREMIUM","membershipStatus":"ACTIVE"}}}`))
	})

	return httptest.NewServer(mux)
}

func jsonDecode(r *http.Request, v interface{}) error {
	// tiny helper to avoid importing encoding/json in three places
	d := jsonNewDecoder(r.Body)
	return d.Decode(v)
}

func TestCheckOneValidCookie(t *testing.T) {
	srv := newStubServer(t, false)
	defer srv.Close()
	prof := &CheckProfile{
		Endpoint: srv.URL + "/api",
		Probes:   []string{"model.summary"},
	}
	client := newCheckClient(prof.timeout())
	res := prof.checkOne(client, "sid=good")
	if res.bucket != bucketValid {
		t.Fatalf("want valid, got %s (%s)", res.bucket, res.out)
	}
	if !strings.Contains(res.out, "region=IN") || !strings.Contains(res.out, "plan=PREMIUM") {
		t.Errorf("missing tags in: %s", res.out)
	}
}

func TestCheckOneInvalidCookie(t *testing.T) {
	srv := newStubServer(t, false)
	defer srv.Close()
	prof := &CheckProfile{Endpoint: srv.URL + "/api", Probes: []string{"model.summary"}}
	client := newCheckClient(prof.timeout())
	res := prof.checkOne(client, "sid=bad123")
	if res.bucket != bucketInvalid {
		t.Fatalf("401 cookie must be invalid, got %s", res.bucket)
	}
}

func TestCheckOneRedirectIsInvalid(t *testing.T) {
	// 302-to-login must be INVALID — validator.py allow_redirects=False semantics
	srv := newStubServer(t, true)
	defer srv.Close()
	prof := &CheckProfile{Endpoint: srv.URL + "/api", Probes: []string{"model.summary"}}
	client := newCheckClient(prof.timeout())
	res := prof.checkOne(client, "sid=dead")
	if res.bucket != bucketInvalid {
		t.Fatalf("redirect (dead cookie) must be invalid, got %s", res.bucket)
	}
}

func TestCheckOneNetworkError(t *testing.T) {
	// endpoint that doesn't exist -> errors bucket (net failure, not invalid)
	prof := &CheckProfile{
		Endpoint:   "http://127.0.0.1:1/nope", // port 1: connection refused
		Probes:     []string{"model.summary"},
		TimeoutSec: 2,
	}
	client := newCheckClient(prof.timeout())
	res := prof.checkOne(client, "sid=good")
	if res.bucket != bucketErrors {
		t.Fatalf("net failure must be errors, got %s", res.bucket)
	}
}

func TestLoginFlowCredsToCookie(t *testing.T) {
	srv := newStubServer(t, false)
	defer srv.Close()
	prof := &CheckProfile{
		Endpoint:      srv.URL + "/api",
		LoginEndpoint: srv.URL + "/login",
		LoginField:    "login",
		PassField:     "password",
		Probes:        []string{"model.summary"},
	}
	client := newCheckClient(prof.timeout())

	// good creds -> login -> cookie -> valid
	res := prof.checkOne(client, "good:secretpass")
	if res.bucket != bucketValid {
		t.Fatalf("good creds via login must be valid, got %s (%s)", res.bucket, res.out)
	}

	// bad creds -> login 401 -> invalid (login-fail)
	res = prof.checkOne(client, "baduser:wrongpass")
	if res.bucket != bucketInvalid {
		t.Fatalf("bad creds via login must be invalid, got %s", res.bucket)
	}
}

func TestCheckChunkOrdering(t *testing.T) {
	// results must come back in input order (validator.py index-mapping semantics)
	srv := newStubServer(t, false)
	defer srv.Close()
	prof := &CheckProfile{Endpoint: srv.URL + "/api", Probes: []string{"model.summary"}, Threads: 8}

	data := "sid=good\nsid=bad\nsid=good\nsid=bad\nsid=good"
	results, valid, invalid, errs := checkChunk(data, prof)

	if valid != 3 || invalid != 2 || errs != 0 {
		t.Fatalf("counts wrong: valid=%d invalid=%d errs=%d", valid, invalid, errs)
	}
	for i, res := range results {
		want := bucketInvalid
		if i%2 == 0 {
			want = bucketValid
		}
		if res.bucket != want {
			t.Errorf("line %d: want %s got %s (ordering broken)", i, want, res.bucket)
		}
	}
}

func TestCheckChunkEmptyLinesSkipped(t *testing.T) {
	srv := newStubServer(t, false)
	defer srv.Close()
	prof := &CheckProfile{Endpoint: srv.URL + "/api", Probes: []string{"model.summary"}}
	results, valid, _, _ := checkChunk("\n\nsid=good\n\n", prof)
	if len(results) != 1 || valid != 1 {
		t.Fatalf("blank lines must be skipped: got %d results, valid=%d", len(results), valid)
	}
}

func TestDigAndTruthy(t *testing.T) {
	data := map[string]interface{}{
		"model": map[string]interface{}{
			"summary": map[string]interface{}{
				"userCountry":       "IN",
				"membershipStatus": "ACTIVE",
				"flag":             false,
				"zero":             float64(0),
			},
		},
	}
	if got := dig(data, "model.summary.userCountry"); got != "IN" {
		t.Errorf("dig userCountry = %v", got)
	}
	if dig(data, "model.summary.missing") != nil {
		t.Error("dig missing path must be nil")
	}
	if dig(data, "bad.path.here") != nil {
		t.Error("dig bad root must be nil")
	}
	if truthy(dig(data, "model.summary.flag")) {
		t.Error("false must not be truthy")
	}
	if truthy(dig(data, "model.summary.zero")) {
		t.Error("0 must not be truthy")
	}
	if !truthy(dig(data, "model.summary.userCountry")) {
		t.Error("IN must be truthy")
	}
}

func TestConcurrentCheckSafety(t *testing.T) {
	// hammer the pool: 200 lines through 30 threads, counts must add up
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":{"summary":{"userCountry":"IN"}}}`))
	}))
	defer srv.Close()

	prof := &CheckProfile{Endpoint: srv.URL + "/api", Probes: []string{"model.summary"}, Threads: 30}
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("sid=good\n")
	}
	_, valid, invalid, errs := checkChunk(sb.String(), prof)
	if valid != 200 || invalid != 0 || errs != 0 {
		t.Fatalf("concurrent run wrong: valid=%d invalid=%d errs=%d", valid, invalid, errs)
	}
}

// ───────────────── real-deployment regression bugs ─────────────────

func TestRateLimitAnd5xxAreErrorsNotInvalid(t *testing.T) {
	// REAL BUG: 429 (rate limited) and 5xx (server hiccup) are TRANSIENT —
	// lumping them into 'invalid' destroys good accounts as false negatives.
	mux := http.NewServeMux()
	mux.HandleFunc("/rate", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) })
	mux.HandleFunc("/oops", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	prof := &CheckProfile{Endpoint: srv.URL + "/rate", Probes: []string{"model.summary"}}
	res := prof.checkOne(newCheckClient(prof.timeout()), "sid=x")
	if res.bucket != bucketErrors {
		t.Errorf("429 must land in errors (retryable), got bucket=%s", res.bucket)
	}

	prof5 := &CheckProfile{Endpoint: srv.URL + "/oops", Probes: []string{"model.summary"}}
	res = prof5.checkOne(newCheckClient(prof5.timeout()), "sid=x")
	if res.bucket != bucketErrors {
		t.Errorf("503 must land in errors (retryable), got bucket=%s", res.bucket)
	}
}

func TestLoginCookieSurvivesExpiresComma(t *testing.T) {
	// REAL BUG: login responses carry 'Set-Cookie: sid=good; Expires=Wed, 21 Oct ...'
	// — the comma inside the Expires date breaks naive split(", ") parsing, so
	// the request sends 'sid=good; Expires=Wed' (corrupted cookie) instead of
	// the clean pair. Stub here parses the Cookie header STRICTLY.
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sid=good; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Path=/")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "sid=good" { // strict: attributes must NOT leak
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":{"summary":{"userCountry":"IN"}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	prof := &CheckProfile{
		Endpoint:      srv.URL + "/api",
		LoginEndpoint: srv.URL + "/login",
		Probes:        []string{"model.summary"},
	}
	res := prof.checkOne(newCheckClient(prof.timeout()), "good:secret")
	if res.bucket != bucketValid {
		t.Errorf("expires-comma login flow must be valid, got %s (out=%s)", res.bucket, res.out)
	}
}

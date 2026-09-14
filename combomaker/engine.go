package main

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ───────────────── byte helpers (zero-alloc) ─────────────────

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

// ───────────────── domain extraction (mirrors combofilter.py exactly) ─────────────────

var twoLevelTLDs = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"co.in": true, "co.jp": true, "or.jp": true, "co.kr": true,
	"com.au": true, "net.au": true, "org.au": true,
	"com.br": true, "com.mx": true, "com.ar": true,
	"co.nz": true, "com.cn": true, "com.tw": true,
	"co.za": true, "com.sg": true, "co.id": true,
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

func registrable(domain string) string {
	labels := strings.Split(domain, ".")
	if len(labels) <= 2 {
		return domain
	}
	two := labels[len(labels)-2] + "." + labels[len(labels)-1]
	if twoLevelTLDs[two] && len(labels) >= 3 {
		return strings.Join(labels[len(labels)-3:], ".")
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

func domainMatches(domain, kw string) bool {
	return domain == kw || strings.HasSuffix(domain, "."+kw)
}

func sanitizeName(name string) string {
	replacer := strings.NewReplacer(
		"\\", "_", "/", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_",
		"|", "_", " ", "_", "\t", "_",
	)
	s := replacer.Replace(name)
	if s == "" {
		s = "unknown"
	}
	return s
}

// ───────────────── engine ─────────────────

type Stats struct {
	Lines     int64
	Hits      int64
	Malformed int64
	Empty     int64
	BytesRead int64
	KwHits    []atomic.Int64
}

type fileEntry struct {
	handle *os.File
	lines  int64
}

func bigRand18() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(900000000000000000))
	return fmt.Sprintf("%d", n.Int64()+100000000000000000)
}

func Run(inputPath string, keywords []string, outDir string, done chan struct{}) *Stats {
	stats := &Stats{KwHits: make([]atomic.Int64, len(keywords))}
	defer close(done)

	kwLower := make([]string, len(keywords))
	kwSanitized := make([]string, len(keywords))
	for i, kw := range keywords {
		kwLower[i] = strings.ToLower(kw)
		kwSanitized[i] = sanitizeName(kwLower[i])
	}

	runID := bigRand18()
	files := make(map[string]*fileEntry)

	defer func() {
		for _, fe := range files {
			fe.handle.Close()
		}
	}()

	getFile := func(name string) *fileEntry {
		if fe, ok := files[name]; ok {
			return fe
		}
		p := filepath.Join(outDir, name+"_"+runID+".txt")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil
		}
		fe := &fileEntry{handle: f}
		files[name] = fe
		return fe
	}

	f, err := os.Open(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening input: %v\n", err)
		close(done)
		return stats
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 8*1024*1024)
	firstLine := true

	for {
		line, err := reader.ReadBytes('\n')
		trimmed := line

		if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\n' {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\r' {
			trimmed = trimmed[:len(trimmed)-1]
		}

		if firstLine {
			if len(trimmed) >= 3 && trimmed[0] == 0xEF && trimmed[1] == 0xBB && trimmed[2] == 0xBF {
				trimmed = trimmed[3:]
			}
			firstLine = false
		}

		atomic.AddInt64(&stats.BytesRead, int64(len(line)))

		if len(trimmed) == 0 {
			if err == io.EOF {
				break
			}
			atomic.AddInt64(&stats.Empty, 1)
			continue
		}

		atomic.AddInt64(&stats.Lines, 1)
		hit := parseLine(trimmed, kwLower, kwSanitized, stats, getFile)

		if !hit {
			atomic.AddInt64(&stats.Malformed, 1)
		}

		if err == io.EOF {
			break
		}
	}

	return stats
}

func parseLine(line []byte, kwLower, kwSanitized []string, stats *Stats, getFile func(string) *fileEntry) bool {
	url, login, pwd := splitCombo(line)
	if len(url) == 0 || len(login) == 0 || len(pwd) == 0 {
		return false
	}

	domain := extractDomain(url)
	if len(domain) == 0 || !strings.Contains(domain, ".") {
		return false
	}

	payload := string(login) + ":" + string(pwd)

	hitAny := false
	for i, kw := range kwLower {
		if domainMatches(domain, kw) {
			if fe := getFile(kwSanitized[i]); fe != nil {
				fmt.Fprintln(fe.handle, payload)
				fe.lines++
			}
			stats.KwHits[i].Add(1)
			hitAny = true
		}
	}

	if hitAny {
		stats.Hits++
	}
	return hitAny
}

func splitCombo(line []byte) (url, login, pwd []byte) {
	n := len(line)
	// Find last colon
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
	// Find second-to-last colon
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

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

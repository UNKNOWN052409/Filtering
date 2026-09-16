package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ───────────────── domain extraction ─────────────────

func TestExtractDomain(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://netflix.com/login", "netflix.com"},
		{"https://www.hbo.com:8080/x", "hbo.com"},
		{"https://user:pass@hbo.com/path", "hbo.com"},
		{"NETFLIX.COM", "netflix.com"},
		{"netflix.com.", "netflix.com"},
		{"http://netflix.com.evil.io", "netflix.com.evil.io"},
		{"ftp://files.spotify.com:2121/a/b", "files.spotify.com"},
	}
	for _, tt := range tests {
		got := extractDomain([]byte(tt.in))
		if got != tt.want {
			t.Errorf("extractDomain(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ───────────────── registrable ─────────────────

func TestRegistrable(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"accounts.netflix.com", "netflix.com"},
		{"mail.yahoo.co.uk", "yahoo.co.uk"},
		{"a.b.yahoo.co.uk", "yahoo.co.uk"},
		{"netflix.com", "netflix.com"},
		{"www.example.com.au", "example.com.au"},
	}
	for _, tt := range tests {
		got := registrable(tt.in)
		if got != tt.want {
			t.Errorf("registrable(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ───────────────── domainMatches ─────────────────

func TestDomainMatches(t *testing.T) {
	tests := []struct {
		dom, kw string
		want    bool
	}{
		{"netflix.com", "netflix.com", true},
		{"accounts.netflix.com", "netflix.com", true},
		{"notnetflix.com", "netflix.com", false},
		{"netflix.com.evil.io", "netflix.com", false},
	}
	for _, tt := range tests {
		got := domainMatches(tt.dom, tt.kw)
		if got != tt.want {
			t.Errorf("domainMatches(%q, %q) = %v, want %v", tt.dom, tt.kw, got, tt.want)
		}
	}
}

// ───────────────── sanitizeName ─────────────────

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"netflix.com", "netflix.com"},
		{"my/file:name", "my_file_name"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		got := sanitizeName(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ───────────────── splitCombo ─────────────────

func TestSplitCombo(t *testing.T) {
	url, login, pwd := splitCombo([]byte("https://netflix.com:user:pass"))
	if string(url) != "https://netflix.com" || string(login) != "user" || string(pwd) != "pass" {
		t.Errorf("splitCombo failed: url=%q login=%q pwd=%q", url, login, pwd)
	}
	// colon in password — rsplit(:, 2) gives url="https://x.com:user", login="pa", pwd="ss"
	url, login, pwd = splitCombo([]byte("https://x.com:user:pa:ss"))
	if string(url) != "https://x.com:user" || string(login) != "pa" || string(pwd) != "ss" {
		t.Errorf("splitCombo with colon in pwd: url=%q login=%q pwd=%q", url, login, pwd)
	}
	// too few colons
	u, l, p := splitCombo([]byte("only:one"))
	if u != nil || l != nil || p != nil {
		t.Error("splitCombo should return nil for 1 colon")
	}
}

// ───────────────── end-to-end Run ─────────────────

func TestRunEndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")
	os.MkdirAll(outDir, 0755)

	lines := []string{
		"https://netflix.com:john:pass1",
		"www.netflix.com:mike:pass2",
		"https://spotify.com:sara:pass3",
		"accounts.netflix.com:u:pass4",
		"malformed_line", // no colons
		"only:one",       // 1 colon
		"",               // empty
	}
	inputPath := filepath.Join(tmpDir, "combos.txt")
	os.WriteFile(inputPath, []byte(strings.Join(lines, "\n")), 0644)

	done := make(chan struct{})
	keywords := []string{"netflix.com", "spotify.com"}
	stats := Run(inputPath, keywords, outDir, done)
	<-done

	if atomicLoadInt64(&stats.Lines) != 6 {
		t.Errorf("Lines = %d, want 6", atomicLoadInt64(&stats.Lines))
	}
	if atomicLoadInt64(&stats.Hits) != 4 {
		t.Errorf("Hits = %d, want 4 (netflix:3 + spotify:1)", atomicLoadInt64(&stats.Hits))
	}
	if atomicLoadInt64(&stats.Malformed) != 2 {
		t.Errorf("Malformed = %d, want 2", atomicLoadInt64(&stats.Malformed))
	}

	// check per-keyword files exist
	for _, kw := range keywords {
		found := false
		filepath.Walk(outDir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.Contains(filepath.Base(p), sanitizeName(kw)+"_") {
				found = true
			}
			return nil
		})
		if !found {
			t.Errorf("no output file found for keyword %q", kw)
		}
	}
}

// ───────────────── BOM handling ─────────────────

func TestRunBOM(t *testing.T) {
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")
	os.MkdirAll(outDir, 0755)

	inputPath := filepath.Join(tmpDir, "bom.txt")
	// UTF-8 BOM + line
	data := []byte{0xEF, 0xBB, 0xBF}
	data = append(data, []byte("https://netflix.com:x:y\n")...)
	os.WriteFile(inputPath, data, 0644)

	done := make(chan struct{})
	stats := Run(inputPath, []string{"netflix.com"}, outDir, done)
	<-done

	if atomicLoadInt64(&stats.Lines) != 1 {
		t.Errorf("BOM: Lines = %d, want 1", atomicLoadInt64(&stats.Lines))
	}
	if atomicLoadInt64(&stats.Hits) != 1 {
		t.Errorf("BOM: Hits = %d, want 1", atomicLoadInt64(&stats.Hits))
	}
}

// ───────────────── tail without newline ─────────────────

func TestRunTailNoNewline(t *testing.T) {
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")
	os.MkdirAll(outDir, 0755)

	inputPath := filepath.Join(tmpDir, "tail.txt")
	os.WriteFile(inputPath, []byte("https://netflix.com:a:b\nhttps://netflix.com:c:d"), 0644)

	done := make(chan struct{})
	stats := Run(inputPath, []string{"netflix.com"}, outDir, done)
	<-done

	if atomicLoadInt64(&stats.Lines) != 2 {
		t.Errorf("tail Lines = %d, want 2", atomicLoadInt64(&stats.Lines))
	}
	if atomicLoadInt64(&stats.Hits) != 2 {
		t.Errorf("tail Hits = %d, want 2", atomicLoadInt64(&stats.Hits))
	}
}

// ───────────────── malformed vs non-matching (regression) ─────────────────

// Regression: valid lines that match no keyword were counted as Malformed
// because parseLine collapsed "unparseable" and "no match" into one false.
func TestRunValidNonMatchingNotMalformed(t *testing.T) {
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")
	os.MkdirAll(outDir, 0755)

	lines := []string{
		"https://netflix.com:a:b",         // match
		"https://notnetflix.com:c:d",      // well-formed, no match — NOT malformed
		"https://netflix.com.evil.io:e:f", // well-formed, no match — NOT malformed
		"bad_line_no_colons",              // malformed
		"only:one",                        // malformed
	}
	inputPath := filepath.Join(tmpDir, "combos.txt")
	os.WriteFile(inputPath, []byte(strings.Join(lines, "\n")), 0644)

	done := make(chan struct{})
	stats := Run(inputPath, []string{"netflix.com"}, outDir, done)
	<-done

	if atomicLoadInt64(&stats.Lines) != 5 {
		t.Errorf("Lines = %d, want 5", atomicLoadInt64(&stats.Lines))
	}
	if atomicLoadInt64(&stats.Hits) != 1 {
		t.Errorf("Hits = %d, want 1", atomicLoadInt64(&stats.Hits))
	}
	if got := atomicLoadInt64(&stats.Malformed); got != 2 {
		t.Errorf("Malformed = %d, want 2 (only unparseable lines)", got)
	}
}

// ───────────────── helpers for tests ─────────────────

func atomicLoadInt64(addr *int64) int64 {
	return *addr
}

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	version = "v1.0.0"
	author  = "@torbug"
	name    = "BugsCloud Combo Maker"
)

func main() {
	enableVTWindows()

	fmt.Printf(
		"==================================================\n"+
			"   %s %s   Made by %s\n"+
			"   url:login:password combo filterer | streaming\n"+
			"==================================================\n\n", name, version, author)

	// ── shared stdin reader (must be created before any prompt) ──
	stdinReader := bufio.NewReader(os.Stdin)

	// ── input path ──
	inputPath := promptLineReader(stdinReader, "Input .txt file path : ")
	if inputPath == "" {
		die("no input file")
	}
	inputPath = strings.Trim(inputPath, "\"' ")
	inputPath = filepath.Clean(inputPath)
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		die(fmt.Sprintf("file not found: %s", inputPath))
	}
	if !strings.HasSuffix(strings.ToLower(inputPath), ".txt") {
		die("only .txt files accepted")
	}
	sz := fileSize(inputPath)
	fmt.Printf("  ✓ size: %s\n\n", humanSize(sz))

	// ── output dir ──
	outDir := promptLineReader(stdinReader, "Save output to (dir) : ")
	if outDir == "" {
		outDir = filepath.Join(".", "results")
	}
	outDir = strings.Trim(outDir, "\"' ")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		die(fmt.Sprintf("cannot create output dir: %v", err))
	}
	fmt.Printf("  ✓ output: %s\n\n", filepath.Clean(outDir))

	// ── keywords ──
	kwLine := promptLineReader(stdinReader, "Target keywords (space/comma separated) : ")
	if kwLine == "" {
		die("at least one keyword required")
	}
	raw := strings.FieldsFunc(kwLine, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == ';'
	})
	var keywords []string
	seen := map[string]bool{}
	for _, r := range raw {
		r = strings.TrimSpace(r)
		r = strings.Trim(r, "\"'")
		if r == "" {
			continue
		}
		r = strings.ToLower(r)
		if !seen[r] {
			seen[r] = true
			keywords = append(keywords, r)
		}
	}
	if len(keywords) == 0 {
		die("at least one keyword required")
	}

	fmt.Printf("  ✓ keywords : %s\n", strings.Join(keywords, ", "))
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Println()

	// ── Ctrl+C graceful shutdown ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n\n⚠ stopped by user")
		os.Exit(0)
	}()

	// ── start processing ──
	fmt.Printf("  ⚙ starting...  input: %s\n\n", filepath.Base(inputPath))
	done := make(chan struct{})
	t0 := time.Now()

	var stats *Stats
	go func() {
		stats = Run(inputPath, keywords, outDir, done)
	}()

	// ── live progress ──
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	quitCh := make(chan bool, 1)

	go func() {
		b, _ := stdinReader.ReadByte()
		if b == 'q' || b == 'Q' || b == 3 {
			quitCh <- true
		}
	}()

firstTick:
	for {
		select {
		case <-ticker.C:
			if stats == nil {
				continue
			}
			elapsed := time.Since(t0).Seconds()
			if elapsed < 0.1 {
				continue
			}
			lines := atomic.LoadInt64(&stats.Lines)
			hits := atomic.LoadInt64(&stats.Hits)
			mal := atomic.LoadInt64(&stats.Malformed)
			speed := float64(lines) / elapsed
			pct := 0.0
			if sz > 0 {
				pct = float64(atomic.LoadInt64(&stats.BytesRead)) / float64(sz) * 100
				if pct > 100 {
					pct = 100
				}
			}
			bar := progressBar(pct/100, 25)

			fmt.Fprintf(os.Stderr, "\r  %s %5.1f%%  lines: %s  %s/s  hits: %s  malformed: %s  t: %s          ",
				bar, pct, fmtN(lines), fmtN(int64(speed)), fmtN(hits), fmtN(mal), fmtDuration(elapsed))

		case <-done:
			break firstTick

		case <-quitCh:
			fmt.Println("\n\n⚠ stopped by user — partial results saved")
			return
		}
	}

	elapsed := time.Since(t0).Seconds()
	lines := atomic.LoadInt64(&stats.Lines)
	hits := atomic.LoadInt64(&stats.Hits)
	mal := atomic.LoadInt64(&stats.Malformed)
	speed := float64(lines) / elapsed

	fmt.Fprint(os.Stderr, "\r"+strings.Repeat(" ", 200)+"\r")
	fmt.Println()
	fmt.Println()
	fmt.Println("==================================================")
	fmt.Printf("  DONE — %s   Made by %s\n", name, author)
	fmt.Println("==================================================")
	fmt.Printf("  Total lines     : %s\n", fmtN(lines))
	fmt.Printf("  Matched (hits)  : %s\n", fmtN(hits))
	fmt.Printf("  Malformed       : %s\n", fmtN(mal))
	fmt.Printf("  Speed           : %s lines/s\n", fmtN(int64(speed)))
	fmt.Printf("  Elapsed         : %s  (%.2fs)\n", fmtDuration(elapsed), elapsed)
	fmt.Println("────────────────────────────────────────────────")
	fmt.Println()
	fmt.Println("  PER-KEYWORD RESULTS:")

	for i, kw := range keywords {
		kh := stats.KwHits[i].Load()
		fmt.Printf("    %-30s : %s hits\n", kw, fmtN(kh))
	}
	fmt.Println()
	fmt.Println("  OUTPUT FILES:")
	filepath.Walk(outDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".txt") && strings.Contains(filepath.Base(p), "_") {
			fmt.Printf("    %s\n", filepath.Clean(p))
		}
		return nil
	})
	fmt.Println()
	fmt.Printf("  [+] Files saved in: %s\n", filepath.Clean(outDir))
	fmt.Printf("  %s %s — Made by %s\n", name, version, author)
	fmt.Println()
}

// ───────────────── helpers ─────────────────

func enableVTWindows() {
	if runtime.GOOS != "windows" {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	consoleHandle, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	var mode uint32
	getConsoleMode.Call(uintptr(consoleHandle), uintptr(unsafe.Pointer(&mode)))
	const ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
	setConsoleMode.Call(uintptr(consoleHandle), uintptr(mode|ENABLE_VIRTUAL_TERMINAL_PROCESSING))
}

func promptLine(prompt string) string {
	return promptLineReader(bufio.NewReader(os.Stdin), prompt)
}

func promptLineReader(r *bufio.Reader, prompt string) string {
	fmt.Print("  " + prompt)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "\n  ✗ %s\n", msg)
	os.Exit(1)
}

func progressBar(frac float64, width int) string {
	filled := int(frac * float64(width))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func fmtN(n int64) string {
	if n < 0 {
		return "0"
	}
	s := fmt.Sprintf("%d", n)
	nLen := len(s)
	if nLen <= 3 {
		return s
	}
	var buf []byte
	rem := nLen % 3
	if rem > 0 {
		buf = append(buf, s[:rem]...)
	}
	for i := rem; i < nLen; i += 3 {
		if len(buf) > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, s[i:i+3]...)
	}
	return string(buf)
}

func fmtDuration(sec float64) string {
	total := int(sec)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func humanSize(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

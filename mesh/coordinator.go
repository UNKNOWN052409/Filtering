package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxRequestBody caps JSON request bodies on the unauthenticated endpoints so
// a caller cannot make the coordinator buffer an arbitrary amount of memory.
const maxRequestBody = 8 << 20

// ───────────────── Coordinator ─────────────────

type Task struct {
	ID         string
	Mode       string // "filter" | "check"
	Keywords   []string
	Profile    *CheckProfile // check mode: endpoint/config for all workers
	Chunk      []byte        // raw combo lines for this chunk
	Lines      int
	Status     string // "pending" | "assigned" | "done"
	Assigned   string // node token
	AssignedAt *time.Time // when it went assigned (drives stale requeue)
	Results    []KeywordResult
}

type Coordinator struct {
	mu           sync.RWMutex
	config       MeshConfig
	tasks        map[string]*Task
	nodes        map[string]*NodeInfo // token → node info
	nodesByEmail map[string]string    // email → token (dedupe: one entry per node)
	totalLines   int64
	totalHits    int64
	pending      chan string // task IDs ready for assignment
}

type NodeInfo struct {
	ID       int
	Email    string
	Token    string
	Hostname string
	Status   string
	LastSeen time.Time
	Lines    int64
	Hits     int64
}

func NewCoordinator(cfg MeshConfig) *Coordinator {
	// a zero interval makes "HeartbeatInterval*3" compare as 0s, so every node
	// reads as dead the instant it is registered — normalise it here, once
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 30
	}
	return &Coordinator{
		config:       cfg,
		tasks:        make(map[string]*Task),
		nodes:        make(map[string]*NodeInfo),
		nodesByEmail: make(map[string]string),
		pending:      make(chan string, 100),
	}
}

func (c *Coordinator) Run(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", c.handleRegister)
	mux.HandleFunc("/api/heartbeat", c.handleHeartbeat)
	mux.HandleFunc("/api/task", c.handleTaskRequest)
	mux.HandleFunc("/api/result", c.handleSubmitResult)
	mux.HandleFunc("/api/upload", c.handleUpload)
	mux.HandleFunc("/api/status", c.handleStatus)
	mux.HandleFunc("/api/ping", c.handlePing)

	// start background jobs
	go c.monitorNodes()
	go c.syncToDrive()
	go c.reapAssigned()

	log.Printf("[coordinator] listening on %s", addr)
	// http.ListenAndServe uses DefaultServer, which sets none of these: a slow
	// or stalled client can hold a connection open indefinitely
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// ── POST /api/register ──
func (c *Coordinator) handleRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	var req RegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad json", 400)
		return
	}

	// validate credentials against config
	valid := false
	for _, n := range c.config.Nodes {
		if n.Email == req.Email && subtle.ConstantTimeCompare([]byte(n.Password), []byte(req.Password)) == 1 {
			valid = true
			break
		}
	}
	if !valid {
		jsonError(w, "invalid credentials", 401)
		return
	}

	token := genToken()
	c.mu.Lock()
	// dedupe: a node that restarts re-registers under the same email — drop the
	// stale token entry so active_nodes doesn't grow one ghost per restart
	if oldTok, exists := c.nodesByEmail[req.Email]; exists {
		delete(c.nodes, oldTok)
	}
	c.nodesByEmail[req.Email] = token
	c.nodes[token] = &NodeInfo{
		ID:       req.NodeID,
		Email:    req.Email,
		Token:    token,
		Hostname: req.Hostname,
		Status:   "idle",
		LastSeen: time.Now(),
	}
	c.mu.Unlock()

	log.Printf("[coordinator] node registered: %s (%s) — token %s", req.Email, req.Hostname, shortID(token, 8))
	jsonResp(w, RegisterResp{OK: true, NodeID: req.NodeID, Token: token, Message: "registered"})
}

// ── POST /api/heartbeat ──
func (c *Coordinator) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req HeartbeatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad json", 400)
		return
	}
	c.mu.Lock()
	if n, ok := c.nodes[req.Token]; ok {
		n.Status = req.Status
		n.LastSeen = time.Now()
		// NOTE: do NOT assign n.Lines/n.Hits here — heartbeats carry no real
		// counts and would clobber the accumulated values from result submits
	}
	c.mu.Unlock()
	jsonResp(w, map[string]bool{"ok": true})
}

// ── GET /api/task ──
func (c *Coordinator) handleTaskRequest(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		jsonError(w, "token required", 401)
		return
	}

	c.mu.RLock()
	_, ok := c.nodes[token]
	c.mu.RUnlock()
	if !ok {
		jsonError(w, "invalid token", 401)
		return
	}

	// find a pending task
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, task := range c.tasks {
		if task.Status == "pending" {
			task.Status = "assigned"
			task.Assigned = token
			now := time.Now()
			task.AssignedAt = &now
			// update node status
			if n, ok := c.nodes[token]; ok {
				n.Status = "busy"
			}
			log.Printf("[coordinator] task %s assigned to node %s", shortID(task.ID, 8), shortID(token, 8))
			jsonResp(w, TaskResp{
				OK:       true,
				TaskID:   task.ID,
				Mode:     task.Mode,
				Keywords: task.Keywords,
				Profile:  task.Profile,
				Data:     string(task.Chunk), // deliver the actual combo lines
				Lines:    task.Lines,
				Message:  "processing",
			})
			return
		}
	}

	jsonResp(w, TaskResp{OK: false, Message: "no tasks available"})
}

// ── POST /api/result ──
func (c *Coordinator) handleSubmitResult(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req ResultSubmit
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad json", 400)
		return
	}

	c.mu.Lock()
	task, exists := c.tasks[req.TaskID]
	// only the node the task was actually assigned to may complete it —
	// otherwise any client that can guess a task ID can mark another node's
	// work done and poison its counts
	if !exists || req.Token == "" || task.Assigned != req.Token {
		c.mu.Unlock()
		jsonError(w, "unknown task or not assigned to this node", 403)
		return
	}
	// only a pending/assigned task counts as a fresh completion — a duplicate
	// or late submit for an already-done task must not double-count
	fresh := task.Status != "done"
	task.Status = "done"
	task.Results = req.Results
	if n, ok := c.nodes[req.Token]; ok {
		n.Status = "idle"
	}
	if fresh {
		c.totalLines += req.Lines
		c.totalHits += req.Hits
		if n, ok := c.nodes[req.Token]; ok {
			n.Lines += req.Lines
			n.Hits += req.Hits
		}
	}
	c.mu.Unlock()

	if fresh {
		log.Printf("[coordinator] task %s done: %d lines, %d hits, %.1fs",
			shortID(req.TaskID, 8), req.Lines, req.Hits, req.Duration)
	}
	jsonResp(w, ResultResp{OK: true, Message: "received"})
}

// ── POST /api/upload (upload combo file) ──
func (c *Coordinator) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}

	// parse multipart: file + keywords
	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100MB max
		jsonError(w, "parse error: "+err.Error(), 400)
		return
	}

	kwStr := r.FormValue("keywords")
	keywords := strings.FieldsFunc(kwStr, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	if len(keywords) == 0 {
		jsonError(w, "keywords required", 400)
		return
	}

	// mode: filter (default) or check — check needs a JSON profile
	mode := r.FormValue("mode")
	if mode == "" {
		mode = "filter"
	}
	if mode != "filter" && mode != "check" {
		jsonError(w, "mode must be filter or check", 400)
		return
	}
	var prof *CheckProfile
	if mode == "check" {
		profStr := r.FormValue("profile")
		prof = &CheckProfile{}
		if profStr != "" {
			if err := json.Unmarshal([]byte(profStr), prof); err != nil {
				jsonError(w, "bad profile json: "+err.Error(), 400)
				return
			}
		}
		if prof.Endpoint == "" {
			jsonError(w, "check mode requires profile.endpoint", 400)
			return
		}
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "file required", 400)
		return
	}
	defer file.Close()

	data, _ := io.ReadAll(file)
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	// split into chunks
	chunkSize := c.config.MaxChunkLines
	if chunkSize <= 0 {
		chunkSize = 50000
	}
	var taskIDs []string
	for i := 0; i < len(lines); i += chunkSize {
		end := i + chunkSize
		if end > len(lines) {
			end = len(lines)
		}
		chunk := lines[i:end]
		// skip empty chunks
		nonEmpty := 0
		for _, l := range chunk {
			if strings.TrimSpace(l) != "" {
				nonEmpty++
			}
		}
		if nonEmpty == 0 {
			continue
		}

		taskID := genToken()
		c.mu.Lock()
		c.tasks[taskID] = &Task{
			ID:       taskID,
			Mode:     mode,
			Keywords: keywords,
			Profile:  prof,
			Chunk:    []byte(strings.Join(chunk, "\n")),
			Lines:    nonEmpty,
			Status:   "pending",
		}
		c.mu.Unlock()
		taskIDs = append(taskIDs, taskID)
	}

	log.Printf("[coordinator] uploaded %d lines → %d chunks (mode=%s), keywords: %s",
		len(lines), len(taskIDs), mode, strings.Join(keywords, ", "))

	jsonResp(w, map[string]interface{}{
		"ok":          true,
		"chunks":      len(taskIDs),
		"total_lines": len(lines),
		"mode":        mode,
		"keywords":    keywords,
	})
}

// ── GET /api/status ──
func (c *Coordinator) handleStatus(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var nodes []NodeStatus
	for _, n := range c.nodes {
		status := n.Status
		if time.Since(n.LastSeen) > time.Duration(c.config.HeartbeatInterval*3)*time.Second {
			status = "dead"
		}
		nodes = append(nodes, NodeStatus{
			ID: n.ID, Email: n.Email, Status: status,
			LastSeen: n.LastSeen, Lines: n.Lines, Hits: n.Hits,
		})
	}

	total, done := 0, 0
	for _, t := range c.tasks {
		total++
		if t.Status == "done" {
			done++
		}
	}

	jsonResp(w, StatusResp{
		OK:          true,
		TotalTasks:  total,
		DoneTasks:   done,
		ActiveNodes: len(c.nodes),
		TotalLines:  c.totalLines,
		TotalHits:   c.totalHits,
		Nodes:       nodes,
	})
}

// ── GET /api/ping ──
func (c *Coordinator) handlePing(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, map[string]interface{}{
		"ok":    true,
		"nodes": len(c.nodes),
		"time":  time.Now().Format(time.RFC3339),
	})
}

// ── background: requeue tasks whose worker vanished mid-run ──
// A crashed worker leaves the task Status=assigned forever; without this the
// task never completes and total counts stall. task_timeout_sec (default 5m).
func (c *Coordinator) reapAssigned() {
	ticker := time.NewTicker(15 * time.Second)
	for range ticker.C {
		c.requeueStale()
	}
}

func (c *Coordinator) requeueStale() {
	timeout := time.Duration(c.config.TaskTimeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	c.mu.Lock()
	for _, t := range c.tasks {
		if t.Status == "assigned" && t.AssignedAt != nil && time.Since(*t.AssignedAt) > timeout {
			t.Status = "pending"
			t.Assigned = ""
			t.AssignedAt = nil
			short := t.ID
			if len(short) > 8 {
				short = short[:8]
			}
			log.Printf("[coordinator] task %s stale — requeued", short)
		}
	}
	c.mu.Unlock()
}

// ── background: monitor dead nodes ──
func (c *Coordinator) monitorNodes() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		c.mu.Lock()
		for token, n := range c.nodes {
			if time.Since(n.LastSeen) > time.Duration(c.config.HeartbeatInterval*3)*time.Second {
				if n.Status != "dead" {
					log.Printf("[coordinator] node %s (%s) marked DEAD — last seen %s ago",
						n.Email, shortID(token, 8), time.Since(n.LastSeen).Round(time.Second))
					n.Status = "dead"
				}
			}
		}
		c.mu.Unlock()
	}
}

// ── background: sync results to Google Drive ──
func (c *Coordinator) syncToDrive() {
	ticker := time.NewTicker(60 * time.Second)
	for range ticker.C {
		// Snapshot under the read lock. Once it is dropped a concurrent requeue or
		// late submit can replace Task.Results while the staging loop below is still
		// ranging over it, so copy the slice and its backing array here.
		type doneSnapshot struct {
			id      string
			results []KeywordResult
		}
		c.mu.RLock()
		doneTasks := []doneSnapshot{}
		for _, t := range c.tasks {
			if t.Status == "done" {
				rs := make([]KeywordResult, len(t.Results))
				copy(rs, t.Results)
				doneTasks = append(doneTasks, doneSnapshot{id: t.ID, results: rs})
			}
		}
		c.mu.RUnlock()
		if len(doneTasks) == 0 {
			continue
		}

		// stage payloads in a transient dir — rebuilt from in-memory task
		// results every pass, so it can never accumulate stale files
		tmpDir := filepath.Join(os.TempDir(), "mesh_results")
		os.MkdirAll(tmpDir, 0755)
		staged := map[string]bool{}
		for _, task := range doneTasks {
			for _, kr := range task.results {
				if len(kr.Payloads) == 0 {
					continue
				}
				fname := fmt.Sprintf("%s_%s.txt", sanitizeName(kr.Keyword), shortID(task.id, 8))
				f, err := os.OpenFile(filepath.Join(tmpDir, fname),
					os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
				if err != nil {
					log.Printf("[coordinator] stage write failed: %v", err)
					continue
				}
				for _, p := range kr.Payloads {
					fmt.Fprintln(f, p)
				}
				f.Close()
			}
			if len(task.results) > 0 {
				staged[task.id] = true
			}
		}

		// nothing staged → nothing to push, safe to drop
		syncOK := len(staged) == 0
		if len(staged) > 0 {
			remote := c.config.RcloneRemote + c.config.DriveSyncPath
			cmd := exec.Command("rclone", "copy", tmpDir, remote, "--update")
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("[coordinator] rclone sync FAILED — keeping %d task(s) for retry: %v — %s",
					len(staged), err, string(output))
			} else {
				syncOK = true
				log.Printf("[coordinator] synced %d task result set(s) to %s", len(staged), remote)
			}
		}
		os.RemoveAll(tmpDir)

		// drop done tasks: all of them once the push succeeded (or there was
		// nothing to push); on failure keep staged tasks so no data is lost
		c.mu.Lock()
		for _, task := range doneTasks {
			if syncOK || !staged[task.id] {
				delete(c.tasks, task.id)
			}
		}
		c.mu.Unlock()
	}
}

// ───────────────── helpers ─────────────────

func genToken() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	return fmt.Sprintf("%x", n.Int64())
}

// shortID returns the first n bytes of s, or all of s when it is shorter.
// It replaces bare s[:n] at logging sites: Go panics when n > len(s), and the
// ids and tokens applied to it are JSON strings (RegisterResp.Token,
// TaskResp.TaskID) that arrive over HTTP and can be empty or short.
func shortID(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func jsonResp(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func sanitizeName(s string) string {
	replacer := strings.NewReplacer(
		"\\", "_", "/", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_",
		"|", "_", " ", "_", "\t", "_",
	)
	return replacer.Replace(s)
}

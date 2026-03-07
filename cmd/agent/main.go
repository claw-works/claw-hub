// claw-hub-agent is a standalone process that connects any agent to the claw-hub network.
// Agents do not need to write any networking code — they just run this binary with handler scripts.
//
// Usage:
//
//	claw-hub-agent \
//	  --hub   http://10.0.1.24:8080 \
//	  --id    <agent-uuid> \
//	  --name  "my-agent" \
//	  --caps  coding,go \
//	  --on-message /path/to/handle-message.sh \
//	  --on-task    /path/to/handle-task.sh
//
// Handler interface (stdin → stdout JSON):
//
//	Message:  stdin  {"type":"agent.message","from":"<id>","text":"..."}
//	          stdout {"reply":"..."}
//
//	Task:     stdin  {"type":"task.assigned","task_id":"...","title":"...","description":"..."}
//	          stdout {"result":"..."} or {"error":"..."}
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ─── CLI flags ────────────────────────────────────────────────────────────────

var (
	flagHub       = flag.String("hub", "http://10.0.1.24:8080", "Hub base URL (http or https)")
	flagID        = flag.String("id", "", "Agent UUID (required)")
	flagName      = flag.String("name", "", "Agent display name (optional)")
	flagCaps      = flag.String("caps", "", "Comma-separated capabilities, e.g. coding,go")
	flagOnMessage = flag.String("on-message", "", "Command to invoke when a message arrives")
	flagOnTask    = flag.String("on-task", "", "Command to invoke when a task is assigned")
	flagVerbose   = flag.Bool("v", false, "Verbose logging")
)

// ─── Protocol types (minimal, matches pkg/protocol) ──────────────────────────

type Envelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	From      string          `json:"from,omitempty"`
	To        string          `json:"to,omitempty"`
	TraceID   string          `json:"trace_id,omitempty"`
	Timestamp time.Time       `json:"ts"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type registerPayload struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

type heartbeatPayload struct {
	AgentID string `json:"agent_id"`
}

type heartbeatACK struct {
	Status string     `json:"status"`
	Inbox  []Envelope `json:"inbox,omitempty"`
}

type taskAssignPayload struct {
	TaskID      string `json:"task_id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type taskUpdatePayload struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type taskResultPayload struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type messagePayload struct {
	Text string `json:"text"`
	From string `json:"from,omitempty"`
}

// ─── Handler stdin/stdout contract ───────────────────────────────────────────

type handlerMessageInput struct {
	Type string `json:"type"`
	From string `json:"from"`
	Text string `json:"text"`
}

type handlerMessageOutput struct {
	Reply string `json:"reply"`
}

type handlerTaskInput struct {
	Type        string `json:"type"`
	TaskID      string `json:"task_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type handlerTaskOutput struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ─── Agent ───────────────────────────────────────────────────────────────────

type Agent struct {
	id       string
	name     string
	caps     []string
	hubBase  string
	onMsg    string // handler command
	onTask   string // handler command
	verbose  bool

	mu   sync.Mutex
	conn *websocket.Conn // nil when not connected via WS
}

func newAgent() *Agent {
	caps := []string{}
	if *flagCaps != "" {
		for _, c := range strings.Split(*flagCaps, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				caps = append(caps, c)
			}
		}
	}
	return &Agent{
		id:      *flagID,
		name:    *flagName,
		caps:    caps,
		hubBase: strings.TrimRight(*flagHub, "/"),
		onMsg:   *flagOnMessage,
		onTask:  *flagOnTask,
		verbose: *flagVerbose,
	}
}

func (a *Agent) logf(format string, args ...interface{}) {
	if a.verbose {
		log.Printf("[agent] "+format, args...)
	}
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (a *Agent) httpPost(path string, body interface{}) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(a.hubBase+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (a *Agent) httpHeartbeat() ([]Envelope, error) {
	data, err := a.httpPost(fmt.Sprintf("/api/v1/agents/%s/heartbeat", a.id), map[string]string{})
	if err != nil {
		return nil, err
	}
	var ack heartbeatACK
	if err := json.Unmarshal(data, &ack); err != nil {
		return nil, err
	}
	return ack.Inbox, nil
}

func (a *Agent) httpSendMessage(toID, text string) error {
	_, err := a.httpPost("/api/v1/messages", map[string]string{
		"from":    a.id,
		"to":      toID,
		"message": text,
	})
	return err
}

func (a *Agent) httpClaimTask(taskID string) error {
	_, err := a.httpPost(fmt.Sprintf("/api/v1/tasks/%s/claim", taskID),
		map[string]string{"agent_id": a.id})
	return err
}

func (a *Agent) httpCompleteTask(taskID, result string) error {
	_, err := a.httpPost(fmt.Sprintf("/api/v1/tasks/%s/complete", taskID),
		map[string]interface{}{"result": result})
	return err
}

func (a *Agent) httpFailTask(taskID, errMsg string) error {
	_, err := a.httpPost(fmt.Sprintf("/api/v1/tasks/%s/fail", taskID),
		map[string]interface{}{"error": errMsg})
	return err
}

// ─── WS send helpers ──────────────────────────────────────────────────────────

func (a *Agent) wsSend(msgType string, payload interface{}) error {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected via WS")
	}
	payBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := Envelope{
		ID:        uuid.New().String(),
		Type:      msgType,
		From:      a.id,
		To:        "hub",
		Timestamp: time.Now(),
		Payload:   payBytes,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// ─── Handler invocation ───────────────────────────────────────────────────────

func invokeHandler(command string, input interface{}) ([]byte, error) {
	inBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal handler input: %w", err)
	}

	parts := strings.Fields(command)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = bytes.NewReader(inBytes)
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("handler exited with error: %w", err)
	}
	return out, nil
}

// ─── Message handling ─────────────────────────────────────────────────────────

func (a *Agent) handleMessage(env Envelope) {
	if a.onMsg == "" {
		a.logf("message from %s (no --on-message handler configured)", env.From)
		return
	}

	var mp messagePayload
	if err := json.Unmarshal(env.Payload, &mp); err != nil {
		log.Printf("[agent] handleMessage: unmarshal payload: %v", err)
		return
	}

	input := handlerMessageInput{
		Type: "agent.message",
		From: env.From,
		Text: mp.Text,
	}

	out, err := invokeHandler(a.onMsg, input)
	if err != nil {
		log.Printf("[agent] handleMessage: handler error: %v", err)
		return
	}

	var result handlerMessageOutput
	if err := json.Unmarshal(out, &result); err != nil {
		log.Printf("[agent] handleMessage: parse handler output: %v", err)
		return
	}

	if result.Reply != "" {
		a.logf("replying to %s: %s", env.From, result.Reply)
		// try WS first, fall back to HTTP
		wsErr := a.wsSend("MESSAGE", messagePayload{Text: result.Reply})
		if wsErr != nil {
			if err := a.httpSendMessage(env.From, result.Reply); err != nil {
				log.Printf("[agent] handleMessage: send reply error: %v", err)
			}
		}
	}
}

// ─── Task handling ────────────────────────────────────────────────────────────

func (a *Agent) handleTask(env Envelope) {
	var tp taskAssignPayload
	if err := json.Unmarshal(env.Payload, &tp); err != nil {
		log.Printf("[agent] handleTask: unmarshal payload: %v", err)
		return
	}

	log.Printf("[agent] task assigned: %s — %s", tp.TaskID, tp.Title)

	// Signal running
	a.sendTaskUpdate(tp.TaskID, "running", "")

	if a.onTask == "" {
		log.Printf("[agent] task %s: no --on-task handler configured, marking failed", tp.TaskID)
		a.sendTaskResult(tp.TaskID, "failed", "", "no --on-task handler configured")
		return
	}

	input := handlerTaskInput{
		Type:        "task.assigned",
		TaskID:      tp.TaskID,
		Title:       tp.Title,
		Description: tp.Description,
	}

	out, err := invokeHandler(a.onTask, input)
	if err != nil {
		log.Printf("[agent] task %s: handler error: %v", tp.TaskID, err)
		a.sendTaskResult(tp.TaskID, "failed", "", err.Error())
		return
	}

	var result handlerTaskOutput
	if err := json.Unmarshal(out, &result); err != nil {
		log.Printf("[agent] task %s: parse handler output: %v", tp.TaskID, err)
		a.sendTaskResult(tp.TaskID, "failed", "", fmt.Sprintf("parse handler output: %v", err))
		return
	}

	if result.Error != "" {
		a.sendTaskResult(tp.TaskID, "failed", "", result.Error)
	} else {
		a.sendTaskResult(tp.TaskID, "done", result.Result, "")
	}
}

func (a *Agent) sendTaskUpdate(taskID, status, msg string) {
	p := taskUpdatePayload{TaskID: taskID, Status: status, Message: msg}
	if err := a.wsSend("TASK_UPDATE", p); err != nil {
		a.logf("sendTaskUpdate via WS failed (%v), falling back to HTTP", err)
		// HTTP fallback for state transitions
		switch status {
		case "running":
			a.httpPost(fmt.Sprintf("/api/v1/tasks/%s/start", taskID), map[string]string{})
		}
	}
}

func (a *Agent) sendTaskResult(taskID, status, result, errMsg string) {
	p := taskResultPayload{TaskID: taskID, Status: status, Result: result, Error: errMsg}
	if err := a.wsSend("TASK_RESULT", p); err != nil {
		a.logf("sendTaskResult via WS failed (%v), falling back to HTTP", err)
		switch status {
		case "done":
			a.httpCompleteTask(taskID, result)
		case "failed":
			a.httpFailTask(taskID, errMsg)
		}
	}
}

// ─── Inbox processing ─────────────────────────────────────────────────────────

func (a *Agent) processEnvelope(env Envelope) {
	a.logf("recv: type=%s from=%s", env.Type, env.From)
	switch env.Type {
	case "task.assigned", "TASK_ASSIGN":
		go a.handleTask(env)
	case "agent.message", "MESSAGE":
		go a.handleMessage(env)
	case "ACK", "PONG", "ERROR":
		// ignore
	default:
		a.logf("unhandled envelope type: %s", env.Type)
	}
}

// ─── WS connection loop ───────────────────────────────────────────────────────

func (a *Agent) wsURL() string {
	base := strings.Replace(a.hubBase, "http://", "ws://", 1)
	base = strings.Replace(base, "https://", "wss://", 1)
	return fmt.Sprintf("%s/ws?agent_id=%s", base, a.id)
}

func (a *Agent) wsConnect(ctx context.Context) error {
	url := a.wsURL()
	a.logf("connecting WS: %s", url)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	log.Printf("[agent] WS connected: %s", url)

	// Send REGISTER
	if err := a.wsSend("REGISTER", registerPayload{
		Name:         a.name,
		Capabilities: a.caps,
	}); err != nil {
		conn.Close()
		return fmt.Errorf("register: %w", err)
	}

	// Heartbeat goroutine
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if err := a.wsSend("HEARTBEAT", heartbeatPayload{AgentID: a.id}); err != nil {
					a.logf("heartbeat error: %v", err)
					return
				}
			}
		}
	}()

	// Read loop
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			a.mu.Lock()
			a.conn = nil
			a.mu.Unlock()
			conn.Close()
			return fmt.Errorf("read: %w", err)
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			a.logf("unmarshal envelope: %v", err)
			continue
		}
		a.processEnvelope(env)
	}
}

// wsLoop tries WS with exponential backoff. Returns only on ctx cancellation.
func (a *Agent) wsLoop(ctx context.Context, wsDown chan<- bool) {
	const maxBackoff = 60 * time.Second
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if attempt > 0 {
			backoff := time.Duration(math.Min(float64(maxBackoff), float64(time.Second)*math.Pow(2, float64(attempt-1))))
			log.Printf("[agent] WS reconnect in %s (attempt %d)…", backoff, attempt+1)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}

		wsDown <- false // signal: attempting WS
		if err := a.wsConnect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[agent] WS error: %v", err)
			wsDown <- true // signal: WS is down, use HTTP polling
		}
		attempt++
	}
}

// ─── HTTP polling fallback ────────────────────────────────────────────────────

func (a *Agent) httpPollLoop(ctx context.Context, wsDown <-chan bool) {
	usePoll := false
	pollTicker := time.NewTicker(15 * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case down := <-wsDown:
			usePoll = down
			if down {
				a.logf("WS down — switching to HTTP polling")
			} else {
				a.logf("WS reconnecting — HTTP polling standby")
				usePoll = false
			}

		case <-pollTicker.C:
			if !usePoll {
				continue
			}
			inbox, err := a.httpHeartbeat()
			if err != nil {
				a.logf("HTTP heartbeat error: %v", err)
				continue
			}
			for _, env := range inbox {
				a.processEnvelope(env)
			}
		}
	}
}

// ─── Run ──────────────────────────────────────────────────────────────────────

func (a *Agent) Run(ctx context.Context) {
	// Initial HTTP heartbeat to confirm connectivity
	if _, err := a.httpHeartbeat(); err != nil {
		log.Printf("[agent] initial heartbeat failed: %v — will retry via WS loop", err)
	} else {
		log.Printf("[agent] initial heartbeat OK")
	}

	wsDown := make(chan bool, 4)
	go a.wsLoop(ctx, wsDown)
	a.httpPollLoop(ctx, wsDown)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()

	if *flagID == "" {
		fmt.Fprintln(os.Stderr, "error: --id is required")
		flag.Usage()
		os.Exit(1)
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	a := newAgent()
	log.Printf("[agent] starting: id=%s caps=%v hub=%s", a.id, a.caps, a.hubBase)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a.Run(ctx)
	log.Printf("[agent] shutdown complete")
}

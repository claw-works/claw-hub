package hub

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// MonitorEvent is the envelope pushed to browser monitor clients over WS.
type MonitorEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
}

type MonitorClient struct {
	id   string
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
}

// MonitorHub manages authenticated browser/monitor WebSocket subscribers.
// It is separate from the agent Hub; it only receives push events.
type MonitorHub struct {
	mu      sync.RWMutex
	clients map[string]*MonitorClient
}

// NewMonitorHub creates a new MonitorHub.
func NewMonitorHub() *MonitorHub {
	return &MonitorHub{clients: make(map[string]*MonitorClient)}
}

// Subscribe registers a new monitor WebSocket connection and returns a client
// that the caller must drive with ReadLoop.
func (mh *MonitorHub) Subscribe(conn *websocket.Conn) *MonitorClient {
	c := &MonitorClient{
		id:   uuid.New().String(),
		conn: conn,
		send: make(chan []byte, 64),
		done: make(chan struct{}),
	}
	mh.mu.Lock()
	mh.clients[c.id] = c
	mh.mu.Unlock()
	go c.writePump()
	return c
}

// Unsubscribe removes the client from the hub.
func (mh *MonitorHub) Unsubscribe(c *MonitorClient) {
	mh.mu.Lock()
	delete(mh.clients, c.id)
	mh.mu.Unlock()
	close(c.done)
}

// Broadcast sends an event to all connected monitor clients.
func (mh *MonitorHub) Broadcast(eventType string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	evt := MonitorEvent{
		ID:        uuid.New().String(),
		Type:      eventType,
		Payload:   json.RawMessage(data),
		Timestamp: time.Now(),
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		return
	}
	mh.mu.RLock()
	defer mh.mu.RUnlock()
	for _, c := range mh.clients {
		select {
		case c.send <- raw:
		default:
			log.Printf("monitor: send buffer full for client %s, dropping event", c.id)
		}
	}
}

// ReadLoop consumes incoming frames (ping/pong keepalive). Blocks until the
// connection is closed, then unsubscribes the client.
func (mh *MonitorHub) ReadLoop(c *MonitorClient) {
	defer func() {
		mh.Unsubscribe(c)
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		return nil
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

// GetID returns the unique client id.
func (c *MonitorClient) GetID() string { return c.id }

func (c *MonitorClient) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case data, ok := <-c.send:
			if !ok {
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

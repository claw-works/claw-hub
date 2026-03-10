// Package hub - room_hub.go
// RoomHub manages per-room WebSocket subscriptions for real-time push.
//
// Supported scenarios:
//  1. Room (group) chat  — clients subscribe to a room_id (e.g. "user:{uid}:default")
//  2. Human single chat  — clients subscribe to their personal inbox room (e.g. "inbox:{uid}")
//
// Both scenarios share the same subscribe/broadcast mechanism;
// the caller controls the room_id naming convention.
package hub

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// RoomClient is a WebSocket subscriber for a specific room.
type RoomClient struct {
	id     string
	roomID string
	conn   *websocket.Conn
	send   chan []byte
	done   chan struct{}
}

// GetID returns the client's unique id.
func (c *RoomClient) GetID() string { return c.id }

// RoomHub manages per-room WebSocket subscriptions.
// Thread-safe; safe to Subscribe/BroadcastToRoom from multiple goroutines.
type RoomHub struct {
	mu    sync.RWMutex
	rooms map[string]map[string]*RoomClient // roomID → clientID → *RoomClient
}

// NewRoomHub creates a new RoomHub.
func NewRoomHub() *RoomHub {
	return &RoomHub{rooms: make(map[string]map[string]*RoomClient)}
}

// Subscribe registers a WebSocket connection for the given room.
// Returns the client; the caller must call ReadLoop(client) to run keepalive.
func (rh *RoomHub) Subscribe(roomID string, conn *websocket.Conn) *RoomClient {
	c := &RoomClient{
		id:     uuid.New().String(),
		roomID: roomID,
		conn:   conn,
		send:   make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	rh.mu.Lock()
	if rh.rooms[roomID] == nil {
		rh.rooms[roomID] = make(map[string]*RoomClient)
	}
	rh.rooms[roomID][c.id] = c
	rh.mu.Unlock()

	go c.writePump()
	return c
}

// unsubscribe removes a client from its room.
func (rh *RoomHub) unsubscribe(c *RoomClient) {
	rh.mu.Lock()
	if room, ok := rh.rooms[c.roomID]; ok {
		delete(room, c.id)
		if len(room) == 0 {
			delete(rh.rooms, c.roomID)
		}
	}
	rh.mu.Unlock()
	// Signal writePump to exit (idempotent).
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// BroadcastToRoom pushes an event to all clients subscribed to the given room.
// Payload is JSON-encoded and wrapped in a MonitorEvent envelope so clients
// receive the same structure as the existing monitor WS stream.
func (rh *RoomHub) BroadcastToRoom(roomID string, eventType string, payload interface{}) {
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

	rh.mu.RLock()
	clients, ok := rh.rooms[roomID]
	if !ok {
		rh.mu.RUnlock()
		return
	}
	// Copy slice to avoid holding lock during send.
	targets := make([]*RoomClient, 0, len(clients))
	for _, c := range clients {
		targets = append(targets, c)
	}
	rh.mu.RUnlock()

	for _, c := range targets {
		select {
		case c.send <- raw:
		default:
			log.Printf("roomhub: send buffer full for client %s in room %s, dropping", c.id, roomID)
		}
	}
}

// CountSubscribers returns the number of active subscribers for a room.
// Useful for diagnostics.
func (rh *RoomHub) CountSubscribers(roomID string) int {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return len(rh.rooms[roomID])
}

// ReadLoop handles keepalive for a room client. It blocks until the connection
// is closed, then unsubscribes the client. The caller should run this after Subscribe.
func (rh *RoomHub) ReadLoop(c *RoomClient) {
	defer func() {
		rh.unsubscribe(c)
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

// writePump sends outgoing messages and periodic pings.
func (c *RoomClient) writePump() {
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

package services

import (
	"db-proxy/internal/core/domain"
	"db-proxy/internal/core/ports"
	"sync"
)

// BroadcastHub coordinates SSE client connections and handles query logging broadcast events.
type BroadcastHub struct {
	mu      sync.RWMutex
	clients map[chan domain.LogEntry]bool
	sysLog  ports.SystemLogger
}

// NewBroadcastHub instantiates a new BroadcastHub.
func NewBroadcastHub(sysLog ports.SystemLogger) *BroadcastHub {
	return &BroadcastHub{
		clients: make(map[chan domain.LogEntry]bool),
		sysLog:  sysLog,
	}
}

// Register registers a client's stream channel.
func (h *BroadcastHub) Register(ch chan domain.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[ch] = true
}

// Unregister removes a client's stream channel.
func (h *BroadcastHub) Unregister(ch chan domain.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.clients[ch]; exists {
		delete(h.clients, ch)
		close(ch)
	}
}

// Broadcast broadcasts a LogEntry to all registered dashboard clients.
// It is non-blocking; if a channel's buffer is full, the message is dropped for that client.
func (h *BroadcastHub) Broadcast(entry domain.LogEntry) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for ch := range h.clients {
		select {
		case ch <- entry:
		default:
			// Non-blocking send: drop if client is too slow or connection is lagging
		}
	}
}

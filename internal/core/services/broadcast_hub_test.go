package services

import (
	"db-proxy/internal/core/domain"
	"testing"
	"time"
)

type dummyLogger struct{}

func (d dummyLogger) Info(msg string, args ...interface{})  {}
func (d dummyLogger) Warn(msg string, args ...interface{})  {}
func (d dummyLogger) Error(msg string, args ...interface{}) {}

func TestBroadcastHub(t *testing.T) {
	logger := dummyLogger{}
	hub := NewBroadcastHub(logger)

	// Create and register a client channel
	clientCh := make(chan domain.LogEntry, 10)
	hub.Register(clientCh)

	entry := domain.LogEntry{
		Timestamp:  time.Now(),
		ThirdParty: "redis",
		ClientIP:   "127.0.0.1",
		Query:      "PING",
	}

	// Broadcast the entry
	hub.Broadcast(entry)

	// Verify the channel receives it
	select {
	case received := <-clientCh:
		if received.Query != "PING" {
			t.Errorf("expected query PING, got %s", received.Query)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for broadcast event")
	}

	// Unregister the channel
	hub.Unregister(clientCh)

	// Verify the channel gets closed
	select {
	case _, open := <-clientCh:
		if open {
			t.Error("expected client channel to be closed after unregistering")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for channel closure")
	}
}

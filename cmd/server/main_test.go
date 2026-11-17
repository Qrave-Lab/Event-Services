package main

import (
	"os"
	"testing"
)

func TestResolveListenAddr(t *testing.T) {
	// Backup environment variables
	origPort := os.Getenv("PORT")
	origEventPort := os.Getenv("EVENT_PORT")
	defer func() {
		os.Setenv("PORT", origPort)
		os.Setenv("EVENT_PORT", origEventPort)
	}()

	tests := []struct {
		name      string
		portEnv   string
		eventPort string
		expected  string
	}{
		{"Both empty", "", "", ":9091"},
		{"Only PORT set", "8080", "", ":8080"},
		{"Only EVENT_PORT set", "", "8081", ":8081"},
		{"Both set (PORT precedence)", "8082", "8083", ":8082"},
		{"Port with colon", ":8084", "", ":8084"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("PORT", tt.portEnv)
			os.Setenv("EVENT_PORT", tt.eventPort)
			
			result := resolveListenAddr()
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	origVal := os.Getenv("TEST_ENV_KEY")
	defer os.Setenv("TEST_ENV_KEY", origVal)

	// Test fallback
	os.Unsetenv("TEST_ENV_KEY")
	if val := envOr("TEST_ENV_KEY", "fallback"); val != "fallback" {
		t.Errorf("Expected fallback, got %s", val)
	}

	// Test actual value
	os.Setenv("TEST_ENV_KEY", "actual")
	if val := envOr("TEST_ENV_KEY", "fallback"); val != "actual" {
		t.Errorf("Expected actual, got %s", val)
	}
}

func TestHubAddRemove(t *testing.T) {
	h := newHub()
	restaurantID := "test-restaurant"
	
	// Internal test without real websocket conn
	// Since websocket.Conn is a struct, we can just pass nil for testing map logic
	
	h.add(restaurantID, nil)
	
	h.mu.RLock()
	room, exists := h.rooms[restaurantID]
	h.mu.RUnlock()
	
	if !exists {
		t.Errorf("Expected room to exist")
	}
	if len(room) != 1 {
		t.Errorf("Expected 1 connection in room, got %d", len(room))
	}

	h.remove(restaurantID, nil)
	
	h.mu.RLock()
	_, exists = h.rooms[restaurantID]
	h.mu.RUnlock()
	
	if exists {
		t.Errorf("Expected room to be deleted after last connection removed")
	}
}

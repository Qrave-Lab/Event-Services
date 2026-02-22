package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"nhooyr.io/websocket"
)

type wsClient struct {
	conn         *websocket.Conn
	restaurantID string
}

type hub struct {
	mu    sync.RWMutex
	rooms map[string]map[*websocket.Conn]struct{}
}

func newHub() *hub {
	return &hub{rooms: make(map[string]map[*websocket.Conn]struct{})}
}

func (h *hub) add(restaurantID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.rooms[restaurantID]; !ok {
		h.rooms[restaurantID] = make(map[*websocket.Conn]struct{})
	}
	h.rooms[restaurantID][conn] = struct{}{}
}

func (h *hub) remove(restaurantID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room, ok := h.rooms[restaurantID]; ok {
		delete(room, conn)
		if len(room) == 0 {
			delete(h.rooms, restaurantID)
		}
	}
}

func (h *hub) broadcast(restaurantID string, payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room := h.rooms[restaurantID]
	for conn := range room {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = conn.Write(ctx, websocket.MessageText, payload)
		cancel()
	}
}

type publishRequest struct {
	RestaurantID string      `json:"restaurant_id"`
	Type         string      `json:"type"`
	Data         interface{} `json:"data"`
}

func main() {
	port := resolveListenAddr()
	jwtSecret := os.Getenv("EVENT_JWT_SECRET")
	publishSecret := os.Getenv("EVENT_SERVICE_SECRET")

	if jwtSecret == "" {
		log.Fatal("EVENT_JWT_SECRET is required")
	}
	if publishSecret == "" {
		log.Fatal("EVENT_SERVICE_SECRET is required")
	}

	h := newHub()

	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event-service"))
	})

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims := jwt.MapClaims{}
		parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
			return []byte(jwtSecret), nil
		})
		if err != nil || !parsed.Valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		restaurantID, _ := claims["restaurant_id"].(string)
		if restaurantID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "connection closed")

		h.add(restaurantID, conn)
		defer h.remove(restaurantID, conn)

		for {
			_, _, err := conn.Read(r.Context())
			if err != nil {
				break
			}
		}

		conn.Close(websocket.StatusNormalClosure, "bye")
	})

	http.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		secret := strings.TrimPrefix(auth, "Bearer ")
		if secret != publishSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req publishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RestaurantID == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		payload, _ := json.Marshal(req)
		h.broadcast(req.RestaurantID, payload)

		w.WriteHeader(http.StatusAccepted)
	})

	fmt.Println("event-service listening on", port)
	log.Fatal(http.ListenAndServe(port, nil))
}

func envOr(key, fallback string) string {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	return val
}

func resolveListenAddr() string {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("EVENT_PORT"))
	}
	if port == "" {
		return ":9091"
	}
	if strings.HasPrefix(port, ":") {
		return port
	}
	return ":" + port
}

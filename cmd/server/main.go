package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/golang-jwt/jwt/v5"
	"nhooyr.io/websocket"
)

var rdb *redis.Client

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

// broadcast sends payload to every connection in the room in parallel, so one
// slow or dead client can't delay everyone else (or other restaurants, since
// this runs on the shared Redis consumer). Clients that fail a write are
// closed; their read loop then removes them.
func (h *hub) broadcast(restaurantID string, payload []byte) {
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.rooms[restaurantID]))
	for conn := range h.rooms[restaurantID] {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()

	for _, conn := range conns {
		go func(c *websocket.Conn) {
			defer repanicReported("broadcast")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := c.Write(ctx, websocket.MessageText, payload); err != nil {
				c.Close(websocket.StatusGoingAway, "write failed")
			}
		}(conn)
	}
}

type publishRequest struct {
	RestaurantID string      `json:"restaurant_id"`
	Type         string      `json:"type"`
	Data         interface{} `json:"data"`
}

func main() {
	flushSentry := initSentry("event-service") // inert without SENTRY_DSN
	port := resolveListenAddr()
	slog.Info("Starting event-service on resolved port", "port", port)

	jwtSecret := os.Getenv("EVENT_JWT_SECRET")
	publishSecret := os.Getenv("EVENT_SERVICE_SECRET")

	redisURL := os.Getenv("REDIS_URL")
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		slog.Error("failed to parse redis url", "error", err)
		os.Exit(1)
	}
	rdb = redis.NewClient(opt)

	if jwtSecret == "" {
		slog.Error("CRITICAL ERROR: EVENT_JWT_SECRET environment variable is missing!")
		os.Exit(1)
	}
	if publishSecret == "" {
		slog.Error("CRITICAL ERROR: EVENT_SERVICE_SECRET environment variable is missing!")
		os.Exit(1)
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
		}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
		if err != nil || !parsed.Valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		restaurantID, _ := claims["restaurant_id"].(string)
		if restaurantID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Patterns match the Origin's host including port (e.g. the POS runs
		// at 127.0.0.1:3010). Override with ALLOWED_ORIGINS (comma-separated).
		// The desktop POS opens this socket from http://127.0.0.1:<port>, so
		// loopback with a port stays allowed everywhere (a valid token is still
		// required). The remaining localhost entries are development only: they
		// need an explicit development APP_ENV, so production fails closed.
		allowedOrigins := []string{
			"127.0.0.1:*",
			"qravetech.in", "*.qravetech.in", "qrave-website.vercel.app", "*.qrave-website.vercel.app",
		}
		switch strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV"))) {
		case "development", "dev", "local", "test":
			allowedOrigins = append(allowedOrigins, "localhost", "localhost:*", "127.0.0.1")
		}
		if envOrigins := os.Getenv("ALLOWED_ORIGINS"); envOrigins != "" {
			allowedOrigins = nil
			for _, o := range strings.Split(envOrigins, ",") {
				if o = strings.TrimSpace(o); o != "" {
					allowedOrigins = append(allowedOrigins, o)
				}
			}
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: allowedOrigins,
		})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "connection closed")

		h.add(restaurantID, conn)
		defer h.remove(restaurantID, conn)

		// Ping regularly so hosting proxies don't drop idle connections and
		// dead clients are noticed.
		pingCtx, stopPing := context.WithCancel(r.Context())
		defer stopPing()
		go func() {
			defer repanicReported("ws-ping")
			t := time.NewTicker(25 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-pingCtx.Done():
					return
				case <-t.C:
					ctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
					err := conn.Ping(ctx)
					cancel()
					if err != nil {
						conn.Close(websocket.StatusGoingAway, "ping timeout")
						return
					}
				}
			}
		}()

		for {
			_, _, err := conn.Read(r.Context())
			if err != nil {

				break

			}
		}

		conn.Close(websocket.StatusNormalClosure, "bye")
	})

	go func() {
		defer repanicReported("redis-subscribe")
		for {
			ctx := context.Background()
			pubsub := rdb.PSubscribe(ctx, "events:restaurant:*")

			slog.Info("Redis Pub/Sub: subscribed to events:restaurant:*")
			ch := pubsub.Channel()

			for msg := range ch {
				parts := strings.Split(msg.Channel, ":")
				if len(parts) == 3 {
					restaurantID := parts[2]
					h.broadcast(restaurantID, []byte(msg.Payload))
				}
			}

			// Channel closed — Redis connection was lost.
			pubsub.Close()
			slog.Warn("Redis Pub/Sub: connection lost — reconnecting in 3s")
			time.Sleep(3 * time.Second)
		}
	}()

	// http.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
	// 	auth := r.Header.Get("Authorization")
	// 	if !strings.HasPrefix(auth, "Bearer ") {
	// 		http.Error(w, "unauthorized", http.StatusUnauthorized)
	// 		return
	// 	}
	// 	secret := strings.TrimPrefix(auth, "Bearer ")
	// 	if secret != publishSecret {
	// 		http.Error(w, "unauthorized", http.StatusUnauthorized)
	// 		return
	// 	}

	// 	var req publishRequest
	// 	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RestaurantID == "" {
	// 		http.Error(w, "invalid request", http.StatusBadRequest)
	// 		return
	// 	}

	// 	payload, _ := json.Marshal(req)
	// 	h.broadcast(req.RestaurantID, payload)

	// 	w.WriteHeader(http.StatusAccepted)
	// })

	fmt.Println("event-service listening on", port)
	if err := http.ListenAndServe(port, withSentry(http.DefaultServeMux)); err != nil {
		slog.Error("server error", "error", err)
		reportPanic(err, nil, "listen")
		flushSentry()
		os.Exit(1)
	}
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
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}

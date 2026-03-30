package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimit returns a simple token-bucket rate limiter middleware.
// It starts a background goroutine to clean up stale entries every minute,
// evicting clients inactive for more than 10 minutes.
func RateLimit(requestsPerSecond int) gin.HandlerFunc {
	type client struct {
		tokens   float64
		lastSeen time.Time
	}

	var mu sync.Mutex
	clients := make(map[string]*client)
	maxTokens := float64(requestsPerSecond)

	// Background cleanup goroutine: every minute, remove entries older than 10 minutes.
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for ip, cl := range clients {
				if cl.lastSeen.Before(cutoff) {
					delete(clients, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return func(c *gin.Context) {
		ip := c.ClientIP()

		mu.Lock()
		cl, ok := clients[ip]
		if !ok {
			cl = &client{tokens: maxTokens, lastSeen: time.Now()}
			clients[ip] = cl
		}

		elapsed := time.Since(cl.lastSeen).Seconds()
		cl.tokens += elapsed * float64(requestsPerSecond)
		if cl.tokens > maxTokens {
			cl.tokens = maxTokens
		}
		cl.lastSeen = time.Now()

		if cl.tokens < 1 {
			mu.Unlock()
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":   "rate_limit_exceeded",
				"message": "too many requests",
			})
			return
		}

		cl.tokens--
		mu.Unlock()

		c.Next()
	}
}

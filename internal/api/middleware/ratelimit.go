package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimit returns a simple token-bucket rate limiter middleware.
func RateLimit(requestsPerSecond int) gin.HandlerFunc {
	type client struct {
		tokens   float64
		lastSeen time.Time
	}

	var mu sync.Mutex
	clients := make(map[string]*client)
	maxTokens := float64(requestsPerSecond)

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

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Server wraps the HTTP server with graceful shutdown.
type Server struct {
	engine     *gin.Engine
	httpServer *http.Server
}

// NewServer creates a new API server.
func NewServer(engine *gin.Engine, host string, port int) *Server {
	return &Server{
		engine: engine,
		httpServer: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", host, port),
			Handler: engine,
		},
	}
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	return s.httpServer.ListenAndServe()
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(shutdownCtx)
}

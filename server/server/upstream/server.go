package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/andydunstall/pico/pkg/conn"
	"github.com/andydunstall/pico/pkg/log"
	"github.com/andydunstall/pico/pkg/rpc"
	"github.com/andydunstall/pico/server/auth"
	proxy "github.com/andydunstall/pico/server/proxy"
	"github.com/andydunstall/pico/server/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

type Proxy interface {
	AddConn(conn proxy.Conn)
	RemoveConn(conn proxy.Conn)
}

// Server is the HTTP server upstream listeners to register endpoints.
type Server struct {
	ln net.Listener

	router *gin.Engine

	httpServer *http.Server
	rpcServer  *rpcServer

	websocketUpgrader *websocket.Upgrader

	proxy Proxy

	shutdownCtx    context.Context
	shutdownCancel func()

	logger log.Logger
}

func NewServer(
	ln net.Listener,
	proxy Proxy,
	verifier auth.Verifier,
	registry *prometheus.Registry,
	logger log.Logger,
) *Server {
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	router := gin.New()
	server := &Server{
		ln:     ln,
		router: router,
		httpServer: &http.Server{
			Addr:    ln.Addr().String(),
			Handler: router,
		},
		rpcServer:         newRPCServer(),
		websocketUpgrader: &websocket.Upgrader{},
		shutdownCtx:       shutdownCtx,
		shutdownCancel:    shutdownCancel,
		proxy:             proxy,
		logger:            logger.WithSubsystem("upstream.server"),
	}

	if verifier != nil {
		tokenMiddleware := middleware.NewAuthMiddleware(verifier, logger)
		router.Use(tokenMiddleware.VerifyEndpointToken)
	}

	// Recover from panics.
	server.router.Use(gin.CustomRecoveryWithWriter(nil, server.panicRoute))

	server.router.Use(middleware.NewLogger(logger))
	if registry != nil {
		router.Use(middleware.NewMetrics("upstream", registry))
	}

	server.registerRoutes()

	return server
}

func (s *Server) Serve() error {
	s.logger.Info("starting http server", zap.String("addr", s.ln.Addr().String()))

	if err := s.httpServer.Serve(s.ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http serve: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) registerRoutes() {
	pico := s.router.Group("/pico/v1")
	pico.GET("/listener/:endpointID", s.listenerRoute)
}

// listenerRoute handles WebSocket connections from upstream listeners.
func (s *Server) listenerRoute(c *gin.Context) {
	endpointID := c.Param("endpointID")

	token, ok := c.Get(middleware.TokenContextKey)
	if ok {
		endpointToken := token.(*auth.EndpointToken)
		if !endpointToken.EndpointPermitted(endpointID) {
			s.logger.Warn(
				"endpoint not permitted",
				zap.Strings("token-endpoints", endpointToken.Endpoints),
				zap.String("endpoint-id", endpointID),
			)
			c.JSON(
				http.StatusUnauthorized,
				gin.H{"error": "endpoint not permitted"},
			)
			return
		}
	}

	wsConn, err := s.websocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade replies to the client so nothing else to do.
		s.logger.Warn("failed to upgrade websocket", zap.Error(err))
		return
	}
	stream := rpc.NewStream(
		conn.NewWebsocketConn(wsConn),
		s.rpcServer.Handler(),
		s.logger,
	)
	defer stream.Close()

	s.logger.Debug(
		"listener connected",
		zap.String("endpoint-id", endpointID),
		zap.String("client-ip", c.ClientIP()),
	)

	conn := proxy.NewRPCConn(endpointID, stream)
	s.proxy.AddConn(conn)
	defer s.proxy.RemoveConn(conn)

	ctx := s.shutdownCtx
	if ok {
		// If the token has an expiry, then we ensure we close the connection
		// to the endpoint once the token expires.
		endpointToken := token.(*auth.EndpointToken)
		if !endpointToken.Expiry.IsZero() {
			var cancel func()
			ctx, cancel = context.WithDeadline(ctx, endpointToken.Expiry)
			defer cancel()
		}
	}

	if err := stream.Monitor(
		ctx,
		time.Second*10,
		time.Second*10,
	); err != nil {
		s.logger.Debug("listener disconnected", zap.Error(err))
	}
}

func (s *Server) panicRoute(c *gin.Context, err any) {
	s.logger.Error(
		"handler panic",
		zap.String("path", c.FullPath()),
		zap.Any("err", err),
	)
	c.AbortWithStatus(http.StatusInternalServerError)
}

func init() {
	// Disable Gin debug logs.
	gin.SetMode(gin.ReleaseMode)
}

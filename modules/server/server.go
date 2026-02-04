package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"gogogo/modules/config"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"gogogo/modules/metrics"
)

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	config     *Config
}

type Config struct {
	Host           string
	Port           int
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration
	MaxHeaderBytes int
	TLSConfig      *tls.Config
	EnableHTTP2    bool
	TCPKeepAlive   time.Duration
}

type Handlers struct {
	Web    http.Handler
	SPA    http.Handler
	Static http.Handler
	API    http.Handler
}

func New(handlers Handlers, cfg config.Config) *Server {
	opts := &Config{
		Host:           cfg.Server.Host,
		Port:           cfg.Server.Port,
		ReadTimeout:    cfg.Server.ReadTimeout,
		WriteTimeout:   cfg.Server.WriteTimeout,
		IdleTimeout:    cfg.Server.IdleTimeout,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
		EnableHTTP2:    cfg.Server.EnableHTTP2,
		TLSConfig:      cfg.Server.TLSConfig,
		TCPKeepAlive:   30 * time.Second,
	}

	mux := http.NewServeMux()

	// API routes
	mux.Handle("/api/", handlers.API)

	// SPA routes
	if handlers.SPA != nil {
		mux.Handle(cfg.URLPrefixes.SPA, http.StripPrefix(cfg.URLPrefixes.SPA, handlers.SPA))
	}

	// Static files
	mux.Handle("/static/", handlers.Static)

	// All other paths go to web handler
	mux.Handle("/", handlers.Web)

	var handler http.Handler = mux

	// Apply metrics if enabled
	if cfg.Server.MetricsEnabled {
		handler = metricsMiddleware(handler)
	}

	if opts.EnableHTTP2 && opts.TLSConfig == nil {
		handler = h2c.NewHandler(handler, &http2.Server{})
	}

	return &Server{
		config: opts,
		httpServer: &http.Server{
			Handler:           handler,
			ReadTimeout:       opts.ReadTimeout,
			WriteTimeout:      opts.WriteTimeout,
			IdleTimeout:       opts.IdleTimeout,
			MaxHeaderBytes:    opts.MaxHeaderBytes,
			ReadHeaderTimeout: opts.ReadTimeout,
		},
	}
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}

	// Divine Speed: Enable TCP Fast Open once on the listener FD
	EnableFastOpen(ln)

	ln = &tcpKeepAliveListener{
		TCPListener:     ln.(*net.TCPListener),
		keepAlivePeriod: s.config.TCPKeepAlive,
	}

	s.listener = ln

	if s.config.TLSConfig != nil {
		s.httpServer.TLSConfig = s.config.TLSConfig
		if s.config.EnableHTTP2 {
			http2.ConfigureServer(s.httpServer, &http2.Server{})
		}
		return s.httpServer.ServeTLS(ln, "", "")
	}

	return s.httpServer.Serve(ln)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

type responseWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		collector := metrics.Get()
		collector.StartRequest()

		rw := &responseWriter{ResponseWriter: w}
		
		next.ServeHTTP(rw, r)

		collector.EndRequest(time.Since(start), uint64(rw.size))
	})
}

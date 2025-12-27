package exporterhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

type Config struct {
	// Address is the listen address, e.g. ":9102" or "0.0.0.0:9102".
	Address string

	// MetricsPath is the path where metrics are served, typically "/metrics".
	MetricsPath string

	// EnablePprof enables /debug/pprof/* handlers.
	EnablePprof bool

	// EnableRoot enables a simple root handler at "/".
	EnableRoot bool

	// EnableHealthz enables "/healthz".
	EnableHealthz bool

	// ReadHeaderTimeout defaults to 5s if zero.
	ReadHeaderTimeout time.Duration

	// Optional TLS config loaded from a "web.config.file" (Prometheus style).
	// If nil, server runs without TLS.
	TLS *TLSConfig
}

type TLSConfig struct {
	CertFile string
	KeyFile  string
}

type Server struct {
	cfg Config
	reg *prometheus.Registry
	mux *http.ServeMux
	srv *http.Server

	startOnce sync.Once
	stopOnce  sync.Once
}

func New(cfg Config, reg *prometheus.Registry) *Server {
	if cfg.Address == "" {
		cfg.Address = ":9102"
	}
	if cfg.MetricsPath == "" {
		cfg.MetricsPath = "/metrics"
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 5 * time.Second
	}
	if reg == nil {
		reg = prometheus.NewRegistry()
	}

	mux := http.NewServeMux()
	mux.Handle(cfg.MetricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	if cfg.EnableHealthz {
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
	}
	if cfg.EnableRoot {
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("tycho exporter\n"))
			_, _ = w.Write([]byte("metrics: " + cfg.MetricsPath + "\n"))
		})
	}
	if cfg.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	srv := &http.Server{
		Addr:              cfg.Address,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	return &Server{cfg: cfg, reg: reg, mux: mux, srv: srv}
}

func (s *Server) Registry() *prometheus.Registry { return s.reg }

// Start runs the HTTP server in a goroutine.
// It returns immediately.
// Shutdown is handled by Stop() (or RunUntilSignal()).
func (s *Server) Start() {
	s.startOnce.Do(func() {
		klog.Infof("[http] serving metrics on %s%s (tls=%v)", s.cfg.Address, s.cfg.MetricsPath, s.cfg.TLS != nil)

		go func() {
			var err error
			if s.cfg.TLS != nil && s.cfg.TLS.CertFile != "" && s.cfg.TLS.KeyFile != "" {
				err = s.srv.ListenAndServeTLS(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
			} else {
				err = s.srv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				klog.Errorf("[http] server error: %v", err)
			}
		}()
	})
}

func (s *Server) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		err = s.srv.Shutdown(ctx)
	})
	return err
}

// RunUntilSignal starts the server and blocks until SIGINT/SIGTERM,
// then shuts down cleanly.
func (s *Server) RunUntilSignal() {
	s.Start()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = s.Stop(ctx)
}

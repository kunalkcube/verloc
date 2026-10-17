package server

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Version and Commit are set by cmd/verloc from ldflags.
var (
	Version = "dev"
	Commit  = "none"
)

func NewServer(cfg *Config) (*Server, error) {
	cfg.applyDefaults()
	backends, err := cfg.resolveBackends()
	if err != nil {
		return nil, err
	}
	catalog.SyncFromConfig(cfg)
	s := &Server{metrics: NewMetrics(), reasonCache: newReasoningCache(maxReasoningEntries), hints: newHintResolver()}
	s.rt.Store(newRuntimeState(cfg, backends))
	s.initGateway()
	return s, nil
}

func Run() {
	configPath := flag.String("config", "config.json", "bootstrap configuration or legacy import file")
	backupDir := flag.String("backup", "", "create a consistent backup directory and exit")
	restoreDir := flag.String("restore", "", "restore a backup into new database paths and exit")
	exportBootstrap := flag.String("export-bootstrap", "", "write a new secret-free bootstrap file after import and exit")
	flag.Parse()

	cfg, err := loadBootstrap(*configPath)
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	if *restoreDir != "" {
		if *backupDir != "" || *exportBootstrap != "" {
			log.Fatal("choose only one of -backup, -restore, or -export-bootstrap")
		}
		if err := restoreGateway(cfg, *restoreDir); err != nil {
			log.Fatalf("restore: %v", err)
		}
		log.Printf("backup restored; start verloc with -config %s", *configPath)
		return
	}
	srv, err := openGateway(cfg, *configPath)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	if *exportBootstrap != "" {
		if *backupDir != "" {
			_ = srv.Close()
			log.Fatal("choose only one of -backup or -export-bootstrap")
		}
		if err := exportBootstrapFile(srv.runtime().cfg, *exportBootstrap); err != nil {
			_ = srv.Close()
			log.Fatalf("export bootstrap: %v", err)
		}
		_ = srv.Close()
		log.Printf("secret-free bootstrap written to %s; archive/remove the legacy secret-bearing configuration separately", *exportBootstrap)
		return
	}
	if *backupDir != "" {
		if err := srv.backupGateway(*backupDir); err != nil {
			_ = srv.Close()
			log.Fatalf("backup: %v", err)
		}
		_ = srv.Close()
		log.Printf("backup saved to %s; keep the master key separately", *backupDir)
		return
	}

	requestContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	httpServer := &http.Server{
		Addr:              srv.runtime().cfg.Listen,
		Handler:           srv.Handler(),
		BaseContext:       func(net.Listener) context.Context { return requestContext },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exited := make(chan error, 1)
	go func() { exited <- httpServer.ListenAndServe() }()
	rt := srv.runtime()
	log.Printf("verloc %s (%s) listening on %s | providers=%d models=%d | dashboard=/dash/",
		Version, Commit, rt.cfg.Listen, len(rt.backends), len(rt.cfg.ModelMap))
	select {
	case err := <-exited:
		if err != nil && err != http.ErrServerClosed {
			_ = srv.Close()
			log.Fatalf("server: %v", err)
		}
	case <-stop.Done():
		srv.shuttingDown.Store(true)
		srv.gatewayConfigChanged()
		if srv.hub != nil {
			srv.hub.Close()
		}
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("[shutdown] draining deadline reached: %v", err)
			cancelRequests()
			_ = httpServer.Close()
		}
		done()
	}
	if err := srv.Close(); err != nil {
		log.Printf("[shutdown] persistence: %v", err)
	}
}

func loadBootstrap(path string) (*Config, error) {
	cfg, err := loadConfig(path)
	if os.IsNotExist(err) {
		cfg, err = &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if len(cfg.Keys) == 0 {
		cfg.Keys = splitComma(os.Getenv("VERLOC_KEYS"))
	}
	base := filepath.Dir(path)
	resolve := func(p, fallback string) string {
		if p == "" {
			p = fallback
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	cfg.ManagementDB = resolve(cfg.ManagementDB, "verloc-data/control.db")
	cfg.MasterKeyFile = resolve(cfg.MasterKeyFile, "verloc-data/master.key")
	cfg.SetupTokenFile = resolve(cfg.SetupTokenFile, "verloc-data/setup.token")
	cfg.Dashboard.DBPath = resolve(cfg.Dashboard.DBPath, "verloc.db")
	return cfg, nil
}

func openGateway(seed *Config, configPath string) (*Server, error) {
	control, err := OpenControlStore(seed.ManagementDB, seed.MasterKeyFile)
	if err != nil {
		return nil, fmt.Errorf("management database: %w", err)
	}
	cfg, err := control.LoadOrImportConfig(seed)
	if err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("management import: %w", err)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		_ = control.Close()
		return nil, err
	}
	srv.control = control
	setupToken, err := LoadOrCreateSetupToken(seed.SetupTokenFile)
	if err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("setup token: %w", err)
	}
	srv.security, err = NewSecurity(control, SecurityOptions{SetupToken: setupToken, SecureCookies: seed.SecureCookies})
	if err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("security: %w", err)
	}
	store, err := OpenStore(cfg.Dashboard.DBPath)
	if err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("analytics database: %w", err)
	}
	hub := NewHub()
	recent := NewRecentLog(cfg.Dashboard.RecentRequests)
	alerts := NewAlertEngine(store, hub, cfg.Dashboard.Alerts)
	srv.SetTelemetry(store, hub, alerts, recent)
	srv.ticker = NewTicker(srv)
	go srv.ticker.Run()
	// Existing installations have raw tick history but no minute rollup. Rebuild
	// it in the background once so the 30-day chart stops re-reading every tick
	// (measured 3.4s / 892MB per poll at 1M ticks). No-op once populated.
	store.EnsureRollupsBackfilled(context.Background())
	log.Printf("[security] local first-run setup token: %s (never share or commit this file)", seed.SetupTokenFile)
	return srv, nil
}

// Handler is the only production HTTP entrypoint. Authentication is enforced
// before dispatch, including for newly added or unknown dashboard API paths.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("POST /responses", s.handleResponses)
	mux.HandleFunc("POST /v1/systemone", s.handleSystemOne)
	mux.HandleFunc("POST /systemone", s.handleSystemOne)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /models", s.handleModels)
	mux.HandleFunc("GET /v1/pricing", s.handlePricing)
	mux.HandleFunc("GET /pricing", s.handlePricing)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /dash/api/state", s.handleDashState)
	mux.HandleFunc("GET /dash/api/series", s.handleDashSeries)
	mux.HandleFunc("GET /dash/api/range", s.handleDashRange)
	mux.HandleFunc("GET /dash/api/recent", s.handleDashRecent)
	mux.HandleFunc("GET /dash/api/requests", s.handleDashRequests)
	mux.HandleFunc("GET /dash/api/analytics", s.handleDashAnalytics)
	mux.HandleFunc("GET /dash/api/alerts", s.handleDashAlerts)
	mux.HandleFunc("GET /dash/api/key-events", s.handleDashKeyEvents)
	mux.HandleFunc("GET /dash/api/live", s.handleDashLive)
	mux.HandleFunc("GET /dash/api/config", s.handleDashConfig)
	mux.HandleFunc("GET /dash/api/admin/providers", s.handleAdminProvidersList)
	mux.HandleFunc("PUT /dash/api/admin/providers", s.handleAdminProviderPut)
	mux.HandleFunc("PUT /dash/api/admin/providers/{name}", s.handleAdminProviderPut)
	mux.HandleFunc("DELETE /dash/api/admin/providers/{name}", s.handleAdminProviderDelete)
	mux.HandleFunc("DELETE /dash/api/admin/providers/{name}/keys/{id}", s.handleAdminProviderKeyDelete)
	mux.HandleFunc("GET /dash/api/admin/models", s.handleAdminModelsList)
	mux.HandleFunc("PUT /dash/api/admin/models", s.handleAdminModelPut)
	mux.HandleFunc("PUT /dash/api/admin/models/{id...}", s.handleAdminModelPut)
	mux.HandleFunc("DELETE /dash/api/admin/models/{id...}", s.handleAdminModelDelete)
	mux.HandleFunc("POST /dash/api/admin/models/bulk", s.handleAdminModelBulk)
	mux.HandleFunc("POST /dash/api/admin/onboard", s.handleAdminOnboard)
	mux.HandleFunc("POST /dash/api/admin/discover", s.handleAdminDiscover)
	mux.HandleFunc("POST /dash/api/admin/test", s.handleAdminTest)
	mux.HandleFunc("POST /dash/api/admin/model-hints", s.handleAdminModelHints)
	mux.HandleFunc("GET /dash/api/admin/settings", s.handleAdminSettings)
	mux.HandleFunc("PUT /dash/api/admin/settings", s.handleAdminSettings)
	mux.HandleFunc("GET /dash/api/admin/audit", s.handleAdminAudit)
	mux.HandleFunc("POST /dash/api/admin/backup", s.handleAdminBackup)
	if s.security != nil {
		s.security.RegisterRoutes(mux)
	}
	mux.HandleFunc("GET /dash/api/", func(w http.ResponseWriter, r *http.Request) { writeErr(w, http.StatusNotFound, "API route not found") })
	mux.HandleFunc("GET /dash", s.handleDashUI)
	mux.HandleFunc("GET /dash/", s.handleDashUI)

	var admin, client http.Handler
	if s.security != nil {
		admin = s.security.AdminMiddleware(mux)
		client = s.security.ClientMiddleware(mux)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		p := r.URL.Path
		if strings.HasPrefix(p, "/dash/api/") || !strings.HasPrefix(p, "/dash") {
			w.Header().Set("Cache-Control", "no-store")
		}
		maxBody := int64(2 << 20)
		if p == "/v1/chat/completions" || p == "/chat/completions" || p == "/v1/responses" || p == "/responses" || p == "/v1/systemone" || p == "/systemone" {
			maxBody = s.runtime().cfg.Limits.MaxBodyBytes
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		switch {
		case p == "/healthz", strings.HasPrefix(p, "/dash/api/auth/"):
			mux.ServeHTTP(w, r)
		case p == "/v1/models", p == "/models":
			// Public catalog. A presented client key still narrows the list to
			// its allowed model scope; anonymous callers see every enabled model.
			if s.security == nil || r.Header.Get("Authorization") == "" {
				mux.ServeHTTP(w, r)
				return
			}
			client.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/dash/api/"), p == "/metrics", p == "/readyz":
			if admin == nil {
				writeErr(w, http.StatusUnauthorized, "administrator session required")
				return
			}
			admin.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/v1/"), p == "/models", p == "/pricing", p == "/chat/completions", p == "/responses", p == "/systemone":
			if client == nil {
				writeErr(w, http.StatusUnauthorized, "gateway API key required")
				return
			}
			client.ServeHTTP(w, r)
		default:
			mux.ServeHTTP(w, r)
		}
	})
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func nowUnix() int64 { return time.Now().Unix() }

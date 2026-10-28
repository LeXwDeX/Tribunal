// Command server runs the M0 review service.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/server"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

const (
	tribunalSessionKeyEnv = "TRIBUNAL_SESSION_KEY"
	legacySessionKeyEnv   = "CRC_SESSION_KEY"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

type options struct {
	configs     []string
	checkConfig bool
	configSet   bool
}

func parseOptions(args []string, output io.Writer) (options, error) {
	opts := options{configs: []string{"config/server.yaml"}}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage: server [--config path ...] [--check-config]")
		fs.PrintDefaults()
	}
	fs.Func("config", "configuration file (repeatable)", func(value string) error {
		if !opts.configSet {
			opts.configs = nil
			opts.configSet = true
		}
		opts.configs = append(opts.configs, value)
		return nil
	})
	fs.BoolVar(&opts.checkConfig, "check-config", false, "validate configuration and exit")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if len(opts.configs) == 0 {
		opts.configs = []string{"config/server.yaml"}
	}
	return opts, nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load(opts.configs...)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger, err := newLogger(stderr)
	if err != nil {
		return err
	}
	warnDeprecatedEnv(logger)
	key, err := decodeMasterKey(cfg.Secrets.MasterKeyEnv)
	if err != nil {
		return err
	}
	adminToken, err := bootstrapToken(cfg.Auth.BootstrapTokenEnv)
	if err != nil {
		return err
	}
	if opts.checkConfig {
		fmt.Fprintln(stdout, "configuration OK")
		return nil
	}

	if err := prepareSQLiteDirectory(cfg.Storage.DSN); err != nil {
		return err
	}
	// Hold the data directory before opening the database so a second
	// instance cannot run recovery, workers or publishing against it.
	dataLock, err := lockSQLiteDirectory(cfg.Storage.DSN)
	if errors.Is(err, sqlite.ErrDataDirLocked) {
		path, _, _ := sqliteFilePath(cfg.Storage.DSN)
		logger.Error("data directory is locked by another Tribunal instance; refusing to start",
			"lock", filepath.Join(filepath.Dir(path), sqlite.LockFileName))
		return errors.New("storage: data directory is locked by another Tribunal instance")
	}
	if err != nil {
		return err
	}
	owner := &resourceOwner{logger: logger}
	if dataLock != nil {
		owner.lock = dataLock
	}
	defer owner.Finish()
	db, err := sqlite.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return errors.New("open storage: SQLite database is unavailable")
	}
	owner.db = db
	logger.Info("storage opened")
	warnUsernameBootstrapAdmins(ctx, logger, cfg.Auth.BootstrapAdmins, db.AdminBindingOwner)

	cipher, err := secret.New(key)
	if err != nil {
		return errors.New("initialize secrets: invalid master key")
	}
	if err := db.ConfigureMCPSecrets(ctx, cipher); err != nil {
		return errors.New("migrate MCP secrets: storage update failed")
	}
	// Bind legacy ciphertexts to their project and field before any service
	// reads them; services only open the sealed format.
	migrated, err := app.MigrateProjectSecrets(ctx, db, cipher, logger)
	if err != nil {
		return errors.New("migrate stored secrets: storage update failed")
	}
	if migrated > 0 {
		logger.Info("stored secrets migrated", "projects", migrated)
	}
	gitlabClient, err := gitlab.New(gitlab.Config{
		BaseURL:        cfg.GitLab.URL,
		RequestTimeout: cfg.GitLab.RequestTimeout,
		MaxConcurrency: cfg.GitLab.MaxConcurrency,
	})
	if err != nil {
		return errors.New("initialize GitLab client: invalid configuration")
	}

	runtime, err := app.NewRuntime(ctx, cfg, app.RuntimeDeps{
		Store:  db,
		Cipher: cipher,
		GitLab: gitlabClient,
		Logger: logger,
	})
	if err != nil {
		return err
	}
	owner.runtime = runtime
	lifecycle := runtime.Lifecycle()

	dispatcher, err := app.NewDispatcher(app.DispatcherConfig{
		Store:     db,
		Workspace: runtime.WorkspaceManager(),
		GitLab:    gitlabClient,
		Cipher:    cipher,
		Profiles:  cfg.Profiles,
		Logger:    logger,
	})
	if err != nil {
		return err
	}
	contexts, err := app.NewContextService(app.ContextServiceConfig{
		Store:     db,
		Workspace: runtime.WorkspaceManager(),
	})
	if err != nil {
		return err
	}
	extensions, err := app.NewExtensionService(app.ExtensionServiceConfig{
		Store:     db,
		MCP:       runtime.MCPManager(),
		Clients:   runtime.LLMClients(),
		Workspace: runtime.WorkspaceManager(),
		DeniedEnv: app.DeniedEnvNames(cfg),
		Logger:    logger,
	})
	if err != nil {
		return err
	}
	sessionKey, err := sessionKeyFromEnv()
	if err != nil {
		return err
	}
	warnEphemeralSessionKey(logger)
	extensionsAPI := server.NewAppExtensions(extensions)
	apiServer, err := server.New(server.Config{
		AdminToken: adminToken,
		Projects:   runtime.Projects(),
		Webhooks:   runtime.Webhooks(),
		Database:   db,
		Worker:     lifecycle,
		Logger:     logger,
		Auth: server.AuthConfig{
			GitLabURL:       cfg.GitLab.URL,
			ClientID:        os.Getenv(cfg.Auth.OAuth.ClientIDEnv),
			ClientSecret:    os.Getenv(cfg.Auth.OAuth.ClientSecretEnv),
			RedirectURL:     strings.TrimRight(cfg.Server.PublicURL, "/") + "/auth/callback",
			SessionKey:      sessionKey,
			SessionTTL:      cfg.Auth.SessionTTL,
			SecureCookies:   secureCookies(cfg.Server.PublicURL),
			BootstrapAdmins: cfg.Auth.BootstrapAdmins,
		},
		Store:      db,
		Reviews:    dispatcher,
		Workspace:  dispatcher,
		Contexts:   contexts,
		Extensions: extensionsAPI,
		MCP:        extensionsAPI,
		LLM:        extensionsAPI,
		Sessions:   db,
		Tokens:     db,
		Access:     db,
		Platform:   cfg,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen: %s", cfg.Server.Listen)
	}
	defer listener.Close()

	worker := jobs.NewWorker(db, jobs.WorkerConfig{
		Handlers:     runtime.Handlers(),
		Workers:      cfg.Jobs.Workers,
		Lease:        cfg.Jobs.Lease,
		PollInterval: time.Second,
		Logger:       logger,
	})
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan error, 1)
	workerFinished := make(chan struct{})
	owner.workerFinished = workerFinished
	started := make(chan struct{})
	go func() {
		lifecycle.MarkStarted()
		close(started)
		err := worker.Run(workerCtx)
		lifecycle.MarkStopped()
		close(workerFinished)
		workerDone <- err
	}()
	<-started

	// Startup recovery runs after the worker owns the queue so recovered
	// workflows can use the same engine and executor registry.
	stopRecovery, recoveryDone := startRecovery(ctx, runtime.Recover, logger)
	owner.recoveryDone = recoveryDone
	stopFeedback, backgroundDone := runtime.StartBackground(ctx)
	owner.backgroundDone = backgroundDone
	defer stopFeedback()

	shutdownTimeout := cfg.Server.WriteTimeout
	if shutdownTimeout <= 0 || shutdownTimeout > 15*time.Second {
		shutdownTimeout = 15 * time.Second
	}
	tracker := newHTTPTracker(apiServer.Handler())
	owner.tracker = tracker
	httpServer := newHTTPServer(cfg.Server, tracker, apiServer.BeginShutdown)
	serveErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	logger.Info("server listening", "listen", cfg.Server.Listen)

	var runErr error
	workerConsumed := false
	httpConsumed := false
	select {
	case <-ctx.Done():
	case err := <-workerDone:
		workerConsumed = true
		if err != nil {
			runErr = err
		} else if ctx.Err() == nil {
			runErr = errors.New("job worker stopped unexpectedly")
		}
	case err := <-serveErr:
		httpConsumed = true
		if err != nil {
			runErr = err
		} else if ctx.Err() == nil {
			runErr = errors.New("HTTP server stopped unexpectedly")
		}
	}

	if err := shutdownHTTPServer(httpServer, shutdownTimeout, logger); err != nil && runErr == nil {
		runErr = err
	}
	tracker.seal()
	stopWorker()
	stopRecovery(shutdownTimeout)
	if !workerConsumed {
		if err := <-workerDone; err != nil && runErr == nil {
			runErr = err
		}
	}
	if !httpConsumed {
		if err := <-serveErr; err != nil && runErr == nil {
			runErr = err
		}
	}
	logger.Info("server stopped")
	return runErr
}

func closeRuntime(runtime io.Closer, logger *slog.Logger) {
	if err := runtime.Close(); err != nil {
		logger.Error("runtime close failed", "error_class", errclass.Of(err))
	}
}

type resourceOwner struct {
	once                         sync.Once
	tracker                      *httpTracker
	workerFinished               <-chan struct{}
	runtime                      io.Closer
	db                           io.Closer
	lock                         interface{ Release() error }
	logger                       *slog.Logger
	recoveryDone, backgroundDone <-chan struct{}
}

func (o *resourceOwner) Finish() {
	o.once.Do(func() {
		if o.runtime != nil {
			closeRuntime(o.runtime, o.logger)
		}
		var handlersDone <-chan struct{}
		if o.tracker != nil {
			handlersDone = o.tracker.seal()
		}
		done := []<-chan struct{}{o.recoveryDone, o.backgroundDone, handlersDone, o.workerFinished}
		finish := func() {
			for _, ch := range done {
				if ch != nil {
					<-ch
				}
			}
			if o.db != nil {
				_ = o.db.Close()
			}
			if o.lock != nil {
				_ = o.lock.Release()
			}
		}
		if o.db == nil && o.lock == nil {
			return
		}
		for _, ch := range done {
			if ch == nil {
				continue
			}
			select {
			case <-ch:
			default:
				go finish()
				return
			}
		}
		finish()
	})
}

type httpTracker struct {
	handler http.Handler
	mu      sync.Mutex
	active  int
	sealed  bool
	done    chan struct{}
}

func newHTTPTracker(handler http.Handler) *httpTracker {
	return &httpTracker{handler: handler, done: make(chan struct{})}
}

func (t *httpTracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	if t.sealed {
		t.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	t.active++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.active--
		if t.sealed && t.active == 0 {
			close(t.done)
		}
	}()
	t.handler.ServeHTTP(w, r)
}

func (t *httpTracker) seal() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sealed {
		t.sealed = true
		if t.active == 0 {
			close(t.done)
		}
	}
	return t.done
}

// startRecovery runs startup recovery in the background. The returned stop
// cancels recovery and waits at most timeout for it to return.
func startRecovery(ctx context.Context, recover func(context.Context) error, logger *slog.Logger) (func(time.Duration), <-chan struct{}) {
	recoveryCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := recover(recoveryCtx); err != nil && recoveryCtx.Err() == nil {
			logger.Warn("startup recovery failed", "error_class", errclass.Of(err))
		}
	}()
	return func(timeout time.Duration) {
		cancel()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			logger.Warn("startup recovery did not stop before shutdown timeout", "timeout", timeout.String())
		}
	}, done
}

// newHTTPServer builds the HTTP server. onShutdown runs when Shutdown starts
// so long-lived responses such as review streams can end; Shutdown itself
// does not cancel active requests.
func newHTTPServer(cfg config.ServerConfig, handler http.Handler, onShutdown func()) *http.Server {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout(cfg.ReadTimeout),
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
	if onShutdown != nil {
		srv.RegisterOnShutdown(onShutdown)
	}
	return srv
}

// shutdownHTTPServer drains connections for at most timeout, then closes the
// rest. A forced close is logged but is still a normal stop: it only cuts
// requests that outlived the grace period.
func shutdownHTTPServer(srv *http.Server, timeout time.Duration, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	logger.Warn("HTTP shutdown timed out; closing remaining connections", "timeout", timeout.String())
	if err := srv.Close(); err != nil {
		return fmt.Errorf("close HTTP server: %w", err)
	}
	return nil
}

// warnDeprecatedEnv tells the operator once per startup which legacy
// compatibility variables are still present. It never logs their values.
func warnDeprecatedEnv(logger *slog.Logger) {
	for _, deprecation := range []struct {
		legacy      string
		replacement string
		detail      string
	}{
		{legacy: "CRC_PUBLIC_URL", replacement: "TRIBUNAL_PUBLIC_URL"},
		{legacy: "CRC_HTTP_PORT", replacement: "TRIBUNAL_HTTP_PORT", detail: " for the host port mapping"},
		{legacy: "CRC_SESSION_KEY", replacement: "TRIBUNAL_SESSION_KEY"},
	} {
		if strings.TrimSpace(os.Getenv(deprecation.legacy)) == "" {
			continue
		}
		logger.Warn("deprecated environment variable is configured; prefer "+deprecation.replacement+deprecation.detail,
			"legacy", deprecation.legacy)
	}
}

// warnUsernameBootstrapAdmins logs one deprecation warning per username-form
// bootstrap_admins entry. It logs the entry index, never the username, and
// names the id:<n> replacement when the username is already bound.
func warnUsernameBootstrapAdmins(ctx context.Context, logger *slog.Logger, entries []string,
	lookup func(context.Context, string) (int64, bool, error)) {
	auth := config.AuthConfig{BootstrapAdmins: entries}
	for _, i := range auth.UsernameBootstrapAdmins() {
		const msg = "auth.bootstrap_admins username entry is deprecated and will be removed in the next major release; use id:<GitLab user id>"
		owner, ok, err := lookup(ctx, strings.ToLower(strings.TrimSpace(entries[i])))
		switch {
		case err != nil:
			logger.Warn(msg, "index", i, "binding", "unavailable")
		case ok:
			logger.Warn(msg, "index", i, "replacement", "id:"+strconv.FormatInt(owner, 10))
		default:
			logger.Warn(msg, "index", i, "binding", "none")
		}
	}
}

// warnEphemeralSessionKey tells the operator that sessions persisted in
// SQLite become unreachable after a restart without a pinned key.
func warnEphemeralSessionKey(logger *slog.Logger) {
	if name, _ := sessionKeySource(); name == "" {
		logger.Warn(tribunalSessionKeyEnv + " is not set; generated an ephemeral session key, so browser sessions will not survive restart")
	}
}

// sessionKeySource prefers TRIBUNAL_SESSION_KEY and falls back to the legacy
// CRC_SESSION_KEY only when the new variable is unset or exactly empty. The
// new value is returned untrimmed so sessionKeyFromEnv can reject a
// whitespace-only value instead of falling back.
func sessionKeySource() (string, string) {
	if raw := os.Getenv(tribunalSessionKeyEnv); raw != "" {
		return tribunalSessionKeyEnv, raw
	}
	if value := strings.TrimSpace(os.Getenv(legacySessionKeyEnv)); value != "" {
		return legacySessionKeyEnv, value
	}
	return "", ""
}

// sessionKeyFromEnv reads the preferred key as base64 (standard or raw) and
// requires at least 32 bytes. An absent or exactly empty key is generated per
// process; stored sessions survive restart but their cookies no longer verify,
// so the caller must warn the operator (see warnEphemeralSessionKey). A
// non-empty TRIBUNAL_SESSION_KEY that is whitespace only, or otherwise
// invalid, is an error and never falls back to the legacy name.
func sessionKeyFromEnv() ([]byte, error) {
	name, value := sessionKeySource()
	value = strings.TrimSpace(value)
	if name == tribunalSessionKeyEnv && value == "" {
		return nil, fmt.Errorf("auth: %s must not be whitespace only", name)
	}
	if value == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.New("auth: cannot generate a session key")
		}
		return key, nil
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("auth: %s must be base64 of at least 32 bytes", name)
	}
	return key, nil
}

func secureCookies(publicURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(publicURL))
	return err == nil && strings.EqualFold(parsed.Scheme, "https")
}

func readHeaderTimeout(readTimeout time.Duration) time.Duration {
	const maximum = 10 * time.Second
	if readTimeout <= 0 || readTimeout > maximum {
		return maximum
	}
	return readTimeout
}

func decodeMasterKey(envName string) ([]byte, error) {
	value := os.Getenv(envName)
	if value == "" {
		return nil, fmt.Errorf("secrets: environment variable %s is not set", envName)
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("secrets: environment variable %s must be base64 of exactly 32 bytes", envName)
	}
	return key, nil
}

func bootstrapToken(envName string) (string, error) {
	value := os.Getenv(envName)
	if len(value) < 32 {
		return "", fmt.Errorf("auth: environment variable %s must be at least 32 bytes", envName)
	}
	return value, nil
}

func newLogger(output io.Writer) (*slog.Logger, error) {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("REVIEW_LOG_LEVEL"))) {
	case "", "info":
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, errors.New("REVIEW_LOG_LEVEL must be debug, info, warn, or error")
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(os.Getenv("REVIEW_LOG_FORMAT"))) {
	case "", "json":
		handler = slog.NewJSONHandler(output, options)
	case "text":
		handler = slog.NewTextHandler(output, options)
	default:
		return nil, errors.New("REVIEW_LOG_FORMAT must be json or text")
	}
	return slog.New(handler).With("service", "review"), nil
}

func prepareSQLiteDirectory(dsn string) error {
	path, memory, err := sqliteFilePath(dsn)
	if err != nil {
		return err
	}
	if memory || path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("storage: cannot create database directory")
	}
	return nil
}

// lockSQLiteDirectory takes the exclusive lock on the directory holding the
// SQLite file. In-memory databases have no directory and return a nil lock.
func lockSQLiteDirectory(dsn string) (*sqlite.DataDirLock, error) {
	path, memory, err := sqliteFilePath(dsn)
	if err != nil {
		return nil, err
	}
	if memory || path == "" {
		return nil, nil
	}
	return sqlite.LockDataDir(filepath.Dir(path))
}

func sqliteFilePath(dsn string) (string, bool, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", false, errors.New("storage: dsn must not be empty")
	}
	if dsn == ":memory:" {
		return "", true, nil
	}
	if !strings.HasPrefix(dsn, "file:") {
		return dsn, false, nil
	}
	rest := strings.TrimPrefix(dsn, "file:")
	pathPart, queryPart, _ := strings.Cut(rest, "?")
	values, err := url.ParseQuery(queryPart)
	if err != nil {
		return "", false, errors.New("storage: invalid SQLite DSN query")
	}
	if values.Get("mode") == "memory" || pathPart == ":memory:" {
		return "", true, nil
	}
	if strings.HasPrefix(pathPart, "//") {
		pathPart = strings.TrimPrefix(pathPart, "//")
		if !strings.HasPrefix(pathPart, "/") {
			return "", false, errors.New("storage: SQLite URI host is not supported")
		}
	}
	if pathPart == "" {
		if values.Get("mode") == "memory" {
			return "", true, nil
		}
		return "", false, errors.New("storage: SQLite DSN has no file path")
	}
	pathPart, err = url.PathUnescape(pathPart)
	if err != nil {
		return "", false, errors.New("storage: invalid SQLite file path")
	}
	return pathPart, false, nil
}

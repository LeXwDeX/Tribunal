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
	"strings"
	"syscall"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/jobs"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/server"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
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
	db, err := sqlite.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return errors.New("open storage: SQLite database is unavailable")
	}
	defer db.Close()
	logger.Info("storage opened")

	cipher, err := secret.New(key)
	if err != nil {
		return errors.New("initialize secrets: invalid master key")
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
	defer runtime.Close()
	lifecycle := runtime.Lifecycle()

	dispatcher, err := app.NewDispatcher(app.DispatcherConfig{
		Store:    db,
		GitLab:   gitlabClient,
		Cipher:   cipher,
		Profiles: cfg.Profiles,
		Logger:   logger,
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
		Workspace: cfg.Workspace.Root,
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
	started := make(chan struct{})
	go func() {
		lifecycle.MarkStarted()
		close(started)
		err := worker.Run(workerCtx)
		lifecycle.MarkStopped()
		workerDone <- err
	}()
	<-started

	// Startup recovery runs after the worker owns the queue so recovered
	// workflows can use the same engine and executor registry.
	go func() {
		if err := runtime.Recover(ctx); err != nil {
			logger.Warn("startup recovery failed", "error", err.Error())
		}
	}()
	stopFeedback := runtime.StartBackground(ctx)
	defer stopFeedback()

	shutdownTimeout := cfg.Server.WriteTimeout
	if shutdownTimeout <= 0 || shutdownTimeout > 15*time.Second {
		shutdownTimeout = 15 * time.Second
	}
	httpServer := &http.Server{
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: readHeaderTimeout(cfg.Server.ReadTimeout),
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
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

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		if runErr == nil {
			runErr = fmt.Errorf("shutdown HTTP server: %w", err)
		}
	}
	cancelShutdown()
	stopWorker()
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

// warnEphemeralSessionKey tells the operator that sessions persisted in
// SQLite become unreachable after a restart without a pinned key.
func warnEphemeralSessionKey(logger *slog.Logger) {
	if strings.TrimSpace(os.Getenv("CRC_SESSION_KEY")) == "" {
		logger.Warn("CRC_SESSION_KEY is not set; generated an ephemeral session key, so browser sessions will not survive restart")
	}
}

// sessionKeyFromEnv reads CRC_SESSION_KEY as base64 (standard or raw) and
// requires at least 32 bytes. An absent key is generated per process; stored
// sessions survive restart but their cookies no longer verify, so the caller
// must warn the operator (see warnEphemeralSessionKey).
func sessionKeyFromEnv() ([]byte, error) {
	value := strings.TrimSpace(os.Getenv("CRC_SESSION_KEY"))
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
		return nil, errors.New("auth: CRC_SESSION_KEY must be base64 of at least 32 bytes")
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

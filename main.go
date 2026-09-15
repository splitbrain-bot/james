// Command james serves a chat agent for one deployed application. It is
// configured by one YAML file and holds no state between requests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"james/internal/agent"
	"james/internal/config"
	"james/internal/llm"
	"james/internal/server"
	"james/internal/tools"
)

// version is the released version. The build sets it with -ldflags.
var version = "dev"

// readHeaderTimeout caps how long a client may take to send its headers.
const readHeaderTimeout = 15 * time.Second

// idleTimeout caps how long a kept-alive connection may stay unused.
const idleTimeout = 120 * time.Second

// shutdownTimeout caps how long running turns may take to finish after a
// signal.
const shutdownTimeout = 20 * time.Second

// shutdownGrace is how long before the shutdown deadline the running turns are
// cancelled, so they can still send their closing event.
const shutdownGrace = 2 * time.Second

// main reads the flags and starts the server.
func main() {
	configPath := flag.String("config", "james.yaml", "path of the configuration file")
	printVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *printVersion {
		fmt.Println(version)
		return
	}
	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "james:", err)
		os.Exit(1)
	}
}

// run loads the configuration, builds the agent and serves until a signal
// asks it to stop.
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.Log)

	provider, err := llm.NewProvider(llm.Options{
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
		Model:    cfg.LLM.Model,
		APIKey:   cfg.LLM.APIKey,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	toolset, closeTools, err := buildTools(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := closeTools.Close(); err != nil {
			logger.Error("cannot close the database connections", "error", err)
		}
	}()

	ag := &agent.Agent{
		Provider:     provider,
		SystemPrompt: cfg.Agent.SystemPrompt,
		Tools:        toolset,
		MaxSteps:     cfg.Agent.MaxToolSteps,
		MaxTokens:    cfg.LLM.MaxTokens,
	}

	handler, err := server.New(cfg, ag, logger)
	if err != nil {
		return err
	}

	// Every request context ends when the server stops, so a running turn
	// hears about the shutdown.
	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	// The answers stream for as long as the model takes, so no write timeout.
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	logger.Info("james is listening",
		"address", listener.Addr().String(),
		"base_path", cfg.Server.BasePath,
		"version", version,
		"tools", toolNames(toolset),
	)

	failed := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	logger.Info("james is shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	stopTurns := time.AfterFunc(shutdownTimeout-shutdownGrace, cancelRequests)
	defer stopTurns.Stop()
	return httpServer.Shutdown(shutdownCtx)
}

// newLogger builds the logger for the configured format. It writes to standard
// output.
func newLogger(cfg config.Log) *slog.Logger {
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

// closeNothing is the closer of a tool set that holds no resource of its own.
type closeNothing struct{}

// Close does nothing and cannot fail.
func (closeNothing) Close() error { return nil }

// buildTools builds the tools of every configured section. A section that is
// absent or empty switches its tools off. The returned closer closes the
// database connections.
func buildTools(ctx context.Context, cfg *config.Config) ([]agent.Tool, io.Closer, error) {
	// The browser tools are built first, so an unknown one stops the start
	// before a database connection is opened.
	browserTools, err := tools.NewBrowserTools(cfg.Tools.Browser)
	if err != nil {
		return nil, closeNothing{}, err
	}

	var all []agent.Tool
	var closeTools io.Closer = closeNothing{}

	if len(cfg.Tools.Files) > 0 {
		all = append(all, tools.NewFileTools(cfg.Tools.Files)...)
	}
	if len(cfg.Tools.Databases) > 0 {
		dbTools, closeDatabases, err := tools.NewDBTools(ctx, cfg.Tools.Databases)
		if err != nil {
			return nil, closeNothing{}, err
		}
		all = append(all, dbTools...)
		closeTools = closeDatabases
	}
	if cfg.Tools.Fetch != nil {
		all = append(all, tools.NewFetchTool(*cfg.Tools.Fetch))
	}
	all = append(all, browserTools...)
	return all, closeTools, nil
}

// toolNames lists the names of the given tools.
func toolNames(toolset []agent.Tool) []string {
	names := make([]string, 0, len(toolset))
	for _, tool := range toolset {
		names = append(names, tool.Name())
	}
	return names
}

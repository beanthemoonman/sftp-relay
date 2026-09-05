// Command relay serves the sftp-relay API and the embedded React bundle.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"sftp-relay/internal/api"
	"sftp-relay/internal/config"
	"sftp-relay/internal/db"
	"sftp-relay/internal/events"
	"sftp-relay/internal/jobs"
	"sftp-relay/internal/nas"
	"sftp-relay/internal/sftpclient"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))
	if cfg.AuthUser == "" || cfg.AuthPass == "" {
		slog.Warn("AUTH_USER/AUTH_PASS are not set: every authenticated route will return 401")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if dir := filepath.Dir(cfg.DBPath); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	openCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	database, err := db.Open(openCtx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func(database *db.DB) {
		err := database.Close()
		if err != nil {
			slog.Error("failed to close database", "err", err)
		}
	}(database)
	slog.Info("database ready", "path", cfg.DBPath)

	pool := sftpclient.NewPool(database, 2*time.Minute, 15*time.Second)
	defer func(pool *sftpclient.Pool) {
		err := pool.Close()
		if err != nil {
			slog.Error("failed to close SFTP client pool", "err", err)
		}
	}(pool)
	nasClient := nas.New(database, cfg.NASSSHKeyPath, 15*time.Second)
	defer func(nasClient *nas.Client) {
		err := nasClient.Close()
		if err != nil {
			slog.Error("failed to close NAS client", "err", err)
		}
	}(nasClient)

	go probeTools(ctx, nasClient)

	hub := events.NewHub()
	manager := jobs.New(database, pool, nasClient, hub)
	if err := manager.Start(ctx); err != nil {
		return err
	}
	defer manager.Stop()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.New(database, pool, nasClient, manager, hub, cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

// probeTools resolves the absolute paths of lftp and sshpass on the NAS and
// caches them. It runs in the background: a NAS that is off or unconfigured
// must not stop the UI from coming up, but the failure is logged loudly with
// the remediation.
func probeTools(ctx context.Context, c *nas.Client) {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch _, err := c.ProbeLftp(probeCtx); {
	case errors.Is(err, nas.ErrNotConfigured):
		slog.Info("NAS is not configured yet; skipping the tool probes")
		return
	case err != nil:
		slog.Error("lftp probe failed; transfers will not run until this is fixed", "err", err)
	}
	// sshpass is only needed for password-authenticated remote servers, so its
	// absence is recorded silently and reported when such a job is queued.
	if path, err := c.ProbeSSHPass(probeCtx); err != nil {
		slog.Debug("sshpass probe failed", "err", err)
	} else if path == "" {
		slog.Info("sshpass is not installed on the NAS; password-authenticated " +
			"remote servers will not transfer until it is")
	}
}

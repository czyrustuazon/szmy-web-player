// Command masterplayer is a self-hosted web music player.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"masterplayer/internal/api"
	"masterplayer/internal/auth"
	"masterplayer/internal/config"
	"masterplayer/internal/errlog"
	"masterplayer/internal/library"
	"masterplayer/internal/store"
	"masterplayer/internal/transcode"
	"masterplayer/internal/upload"
	"masterplayer/web"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit (used by the Docker HEALTHCHECK)")
	flag.Parse()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}
	if *healthcheck {
		os.Exit(probe(cfg.Port))
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func probe(port int) int {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run(cfg config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	logger := errlog.New(cfg.LogPath(), 1<<20, os.Stderr)

	st, err := store.Open(cfg.StatePath())
	if err != nil {
		return err
	}
	lib, err := library.New(cfg.MusicDir, "", cfg.ReadOnly)
	if err != nil {
		return err
	}
	if lib.ReadOnly() {
		fmt.Println("library is read-only (MP_READ_ONLY or an unwritable music directory): delete and upload are disabled")
	}
	a, notice, err := setupAuth(cfg)
	if err != nil {
		return err
	}

	var tx *transcode.Service
	if bin, err := exec.LookPath(cfg.VgmstreamBin); err == nil {
		tx, err = transcode.New(transcode.VGMStream{Bin: bin}, cfg.CacheDir(), cfg.TranscodeWorkers, cfg.CacheMB<<20)
		if err != nil {
			return fmt.Errorf("transcode cache: %w", err)
		}
	} else {
		fmt.Printf("warning: %q not found; BRSTM/BCSTM/BFSTM and other game formats will not play\n", cfg.VgmstreamBin)
	}

	up := upload.New(lib.Root(), cfg.UploadSubdir, cfg.MaxUploadMB<<20, cfg.MinFreeMB<<20)
	srv := api.New(api.Deps{Cfg: cfg, Lib: lib, Store: st, Auth: a, TX: tx, Up: up, Log: logger, Static: web.FS})
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go purgeLoop(ctx, lib, time.Duration(cfg.TrashMinutes)*time.Minute, logger)
	go up.RunJanitor(ctx, time.Duration(cfg.UploadTTLHours)*time.Hour, time.Hour)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	fmt.Printf("masterplayer listening on http://localhost:%d (music: %s)\n", cfg.Port, cfg.MusicDir)
	if notice != "" {
		fmt.Println(notice)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func purgeLoop(ctx context.Context, lib *library.Library, maxAge time.Duration, logger *errlog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := lib.PurgeTrash(maxAge); err != nil {
				logger.Append(errlog.CodeIO, "purge", "", err.Error())
			}
		}
	}
}

const sessionTTL = 30 * 24 * time.Hour

// setupAuth turns the login on when MP_ADMIN_PASSWORD is set. With a blank
// password there is no login at all (open access) and a warning is returned.
func setupAuth(cfg config.Config) (*auth.Auth, string, error) {
	if cfg.AuthDisabled {
		return auth.New(nil, nil, true, sessionTTL),
			"WARNING: MP_ADMIN_PASSWORD is blank, so there is NO LOGIN: anyone who can reach this port can play, upload and delete. Set a password to turn the login on.", nil
	}
	salt, err := auth.NewSalt()
	if err != nil {
		return nil, "", err
	}
	return auth.New(salt, auth.Hash(salt, cfg.AdminPassword), false, sessionTTL), "", nil
}

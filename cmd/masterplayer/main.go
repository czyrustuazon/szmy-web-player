// Command masterplayer is a self-hosted web music player.
package main

import (
	"context"
	"encoding/hex"
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
	lib, err := library.New(cfg.MusicDir, "")
	if err != nil {
		return err
	}
	if lib.ReadOnly() {
		fmt.Println("music directory is read-only: delete and upload are disabled")
	}
	a, notice, err := setupAuth(cfg, st)
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

	srv := api.New(api.Deps{Cfg: cfg, Lib: lib, Store: st, Auth: a, TX: tx, Log: logger, Static: web.FS})
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go purgeLoop(ctx, lib, time.Duration(cfg.TrashMinutes)*time.Minute, logger)

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

// setupAuth picks the password source: disabled, MP_ADMIN_PASSWORD, a stored
// hash, or a freshly generated password that is printed once.
func setupAuth(cfg config.Config, st *store.Store) (*auth.Auth, string, error) {
	if cfg.AuthDisabled {
		return auth.New(nil, nil, true, sessionTTL), "authentication is DISABLED (MP_AUTH_DISABLED)", nil
	}
	if cfg.AdminPassword != "" {
		salt, err := auth.NewSalt()
		if err != nil {
			return nil, "", err
		}
		return auth.New(salt, auth.Hash(salt, cfg.AdminPassword), false, sessionTTL), "", nil
	}
	saltHex, hashHex := st.Password()
	if saltHex != "" && hashHex != "" {
		salt, err1 := hex.DecodeString(saltHex)
		hash, err2 := hex.DecodeString(hashHex)
		if err1 == nil && err2 == nil {
			return auth.New(salt, hash, false, sessionTTL), "", nil
		}
	}
	pw, err := auth.GeneratePassword(10)
	if err != nil {
		return nil, "", err
	}
	salt, err := auth.NewSalt()
	if err != nil {
		return nil, "", err
	}
	hash := auth.Hash(salt, pw)
	if err := st.SetPassword(hex.EncodeToString(salt), hex.EncodeToString(hash)); err != nil {
		return nil, "", err
	}
	return auth.New(salt, hash, false, sessionTTL),
		fmt.Sprintf("generated login password (shown once; set MP_ADMIN_PASSWORD to choose your own): %s", pw), nil
}

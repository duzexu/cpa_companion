package companion

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func Run() error {
	cfg, err := loadSettings()
	if err != nil {
		return err
	}
	if err := cfg.initializeSecrets(); err != nil {
		return err
	}
	db, err := openStore(filepath.Join(cfg.DataDir, "store.json"))
	if err != nil {
		return err
	}
	defer db.Close()
	if len(db.auditSettings().Endpoints) == 0 {
		return errors.New("audit settings require at least one endpoint")
	}
	logs, err := openUsageLogs(cfg.DataDir)
	if err != nil {
		return err
	}
	defer logs.Close()
	app := newApp(cfg, db)
	app.logs = logs
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// WriteTimeout stays unset for streaming responses; request bodies get a
	// per-request read deadline in withSecurity instead of ReadTimeout.
	server := &http.Server{Addr: cfg.Listen, Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	log.Printf("companion forwarding to CPA at %s", cfg.CPABaseURL.Host)
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	return server.Shutdown(shutdownCtx)
}

// Command switch runs the AppTestify UPI Switch: API gateway, orchestrator, bank callback listener,
// background workers and admin console in one horizontally scalable process.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/AppTestify/UPI-Switch/internal/app"
	"github.com/AppTestify/UPI-Switch/internal/config"
)

func main() {
	cfg := config.LoadSwitch()
	var lvl slog.Level
	lvl.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, app.Options{Logger: log})
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer a.Close()
	a.Start(ctx)

	tlsCfg, err := app.ServerTLS(cfg)
	if err != nil {
		log.Error("tls config", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr: cfg.ListenAddr, Handler: a.Server.Handler(), TLSConfig: tlsCfg,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
	}
	go func() {
		log.Info("switch listening", "addr", cfg.ListenAddr, "tls", tlsCfg != nil)
		if tlsCfg != nil {
			err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	log.Info("switch stopped")
}

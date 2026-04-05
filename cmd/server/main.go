package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k3s-dashboard/internal/api"
	"k3s-dashboard/internal/collector"
	"k3s-dashboard/internal/config"
	"k3s-dashboard/internal/kube"
	"k3s-dashboard/internal/storage"
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	if err := run(logger); err != nil {
		logger.Fatalf("server exited with error: %v", err)
	}
}

func run(logger *log.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	kubeConfig, err := kube.BuildConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}

	store, err := storage.Open(cfg.SQLitePath, cfg.Retention, cfg.FailIfDataDirExists)
	if err != nil {
		return err
	}
	defer store.Close()

	metricCollector, err := collector.New(kubeConfig, store, cfg.ScrapeInterval, logger)
	if err != nil {
		return err
	}

	go metricCollector.Run(ctx)

	handler := api.NewServer(store, metricCollector, logger).Routes()
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Printf("http shutdown error: %v", err)
		}
	}()

	logger.Printf("listening on %s", cfg.ListenAddr)

	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err

}
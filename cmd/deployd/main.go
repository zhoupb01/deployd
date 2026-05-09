package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zhoupb01/deployd/internal/auth"
	"github.com/zhoupb01/deployd/internal/config"
	"github.com/zhoupb01/deployd/internal/deploy"
	"github.com/zhoupb01/deployd/internal/server"
)

func main() {
	configPath := flag.String("config", "/etc/deployd/config.yaml", "path to config file")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("deployd: load config: %v", err)
	}

	deployer, err := deploy.New(cfg)
	if err != nil {
		log.Fatalf("deployd: init deployer: %v", err)
	}
	verifier := auth.NewVerifier()
	srv := server.New(cfg, deployer, verifier)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("deployd: listening on %s with %d service(s)", cfg.Listen, len(cfg.Services))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("deployd: http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("deployd: shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("deployd: graceful shutdown failed: %v", err)
		os.Exit(1)
	}
	log.Printf("deployd: bye")
}

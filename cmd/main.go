package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/restartfu/decryptmypack/internal"
	"github.com/restartfu/decryptmypack/internal/adapters/downloader"
	"github.com/restartfu/decryptmypack/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	login := flag.Bool("login", false, "sign in the service's own Microsoft account, save it to TOKEN_PATH, and exit")
	flag.Parse()

	cfg, err := config.FromEnv(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if *login {
		if err := downloader.Login(cfg.TokenPath); err != nil {
			log.Fatal(err)
		}
		log.Printf("Service token saved to %s", cfg.TokenPath)
		return
	}

	s, err := internal.Assemble(cfg)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Server listening on %s\n", cfg.Addr)
	if err := s.StartContext(ctx, cfg.Addr); err != nil {
		log.Fatal(err)
	}
}

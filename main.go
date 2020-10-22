package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Pippolo84/rss-service/service"
)

// Some feed to use as tests:
//
// http://joeroganexp.joerogan.libsynpro.com/rss
// https://rss.art19.com/1619
// https://feeds.npr.org/510312/podcast.xml
// https://feeds.megaphone.fm/ADL9840290619

// SvcCooldownTimeout is the maximum time we want to wait for the service cooldown
const SvcCooldownTimeout time.Duration = 10 * time.Second

func main() {
	svc := service.NewService(":8080")

	var initWg sync.WaitGroup
	initWg.Add(1)

	errs := svc.Run(&initWg)

	// wait for service initialization
	initWg.Wait()

	// trap incoming SIGINT and SIGKTERM
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// block until a signal or an error is received
	select {
	case err := <-errs:
		log.Println(err)
	case sig := <-signalChan:
		log.Printf("got signal: %v, shutting down...\n", sig)
	}

	// graceful shutdown the service
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), SvcCooldownTimeout)
	defer cancelShutdown()

	var stopWg sync.WaitGroup
	stopWg.Add(1)

	if err := svc.Shutdown(shutdownCtx, &stopWg); err != nil {
		log.Println(err)
	}

	// wait for service to cleanup everything
	stopWg.Wait()
}

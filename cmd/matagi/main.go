package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/yohn-jp/matagi/internal/api"
	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/runtime"
)

func main() {
	store, err := config.NewUserStore()
	if err != nil {
		log.Fatal(err)
	}
	rt, err := runtime.New(store)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- api.Serve(ctx, rt) }()
	select {
	case err := <-done:
		if err != nil {
			log.Print(err)
		}
		stop()
		<-serverDone
	case err := <-serverDone:
		if err != nil {
			log.Print(err)
		}
		stop()
		<-done
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Close(shutdown); err != nil {
		log.Print(err)
	}
}

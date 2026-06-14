package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"battery-rental/internal/api"
	"battery-rental/internal/config"
	"battery-rental/internal/database"
	"battery-rental/internal/middleware"
	"battery-rental/internal/redisx"

	"github.com/gin-gonic/gin"
)

func main() {
	if err := config.Load(); err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := database.Connect(); err != nil {
		log.Fatalf("connect db: %v", err)
	}

	if err := database.Migrate(); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	if err := database.Seed(); err != nil {
		log.Fatalf("seed: %v", err)
	}

	if err := redisx.Connect(); err != nil {
		log.Fatalf("connect redis: %v", err)
	}

	gin.SetMode(config.AppConfig.ServerMode)
	e := gin.New()
	e.Use(gin.Logger())
	e.Use(middleware.Recovery())
	e.Use(middleware.CORSMiddleware())
	e.Use(middleware.RequestID())
	e.Use(middleware.CaptureResponseBody())

	r := api.NewRouter()
	r.RegisterRoutes(e)

	srv := &http.Server{
		Addr:         ":" + config.AppConfig.ServerPort,
		Handler:      e,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		log.Printf("[Server] starting on :%s", config.AppConfig.ServerPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[Server] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("[Server] stopped")
}

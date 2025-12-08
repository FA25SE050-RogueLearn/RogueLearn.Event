package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"runtime/debug"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/cmd/api"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/database"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/protos"
	"google.golang.org/grpc"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/executor"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/rabbitmq"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/user"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/handlers"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/service"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/env"
	"github.com/joho/godotenv"
	"github.com/lmittmann/tint"
)

func main() {
	// Load .env file for development environment
	// In production (Docker Swarm), this will fail silently and use Docker secrets instead
	err := godotenv.Load()
	if err != nil {
		// Only log as info since this is expected in production
		log.Printf("Info: .env file not loaded (this is normal in production): %v", err)
	} else {
		log.Printf("Development mode: loaded .env file")
	}

	httpPort := env.GetInt("EVENT_HTTP_PORT", 8084)
	gRPCPort := env.GetInt("EVENT_GRPC_PORT", 8085)

	cfg := &api.Config{
		HttpPort: httpPort,
		GrpcPort: gRPCPort,
	}

	connStr := env.GetString("EVENT_SUPABASE_DB_CONNSTR", "")
	if connStr == "" {
		print("EVENT_SUPABASE_DB_CONNSTR environment variable is not set")
		panic("EVENT_SUPABASE_DB_CONNSTR environment variable is not set")
	}

	db, err := database.NewPool(connStr)
	if err != nil {
		panic(err)
	}

	queries := store.New(db)

	// log to os standard output
	slogHandler := tint.NewHandler(os.Stdout, &tint.Options{Level: slog.LevelDebug, AddSource: true})
	logger := slog.New(slogHandler)
	slog.SetDefault(logger) // Set default for any library using slog's default logger

	rabbitMQURL := env.GetString("EVENT_RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	rabbitClient, err := rabbitmq.NewRabbitMQClient(rabbitMQURL, logger)
	if err != nil {
		print("Could not connect to RabbitMQ", rabbitMQURL)
		panic(fmt.Sprintf("Could not connect to RabbitMQ: %v", err))
	}
	defer rabbitClient.Close()

	executorURL := env.GetString("EVENT_EXECUTOR_URL", "")
	if executorURL == "" {
		print("EVENT_EXECUTOR_URL environment variable is not set")
		panic("EVENT_EXECUTOR_URL environment variable is not set")
	}
	executorClient, err := executor.NewClient(executorURL)
	if err != nil {
		print("Could not connect to Executor", executorURL)
		panic(fmt.Sprintf("Could not connect to Executor: %v", err))
	}
	defer executorClient.Close()

	userURL := env.GetString("EVENT_USER_URL", "")
	if userURL == "" {
		print("EVENT_USER_URL environment variable is not set")
		panic("EVENT_USER_URL environment variable is not set")
	} else {
		fmt.Println("User URL:", userURL)
	}

	userClient, err := user.NewClient(userURL)
	if err != nil {
		print("Could not connect to User", userURL)
		panic(fmt.Sprintf("Could not connect to User Service: %v", err))
	}
	defer userClient.Close()
	// Create a context for background goroutines
	ctx := context.Background()

	// Create handler repo with context for cleanup routine
	handlerRepo := handlers.NewHandlerRepo(ctx, logger, db, queries, rabbitClient, executorClient, userClient)

	app := api.NewApplication(cfg, logger, queries, handlerRepo)

	// run grpc server
	lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", cfg.GrpcPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	protos.RegisterEventServiceServer(grpcServer, service.NewEventServiceServer(queries, logger))

	go grpcServer.Serve(lis)
	logger.Info("GRPC server started successfully", "port", cfg.GrpcPort)

	// run HTTP server
	err = app.Run()
	if err != nil {
		// Using standard log here to be absolutely sure it prints if slog itself had an issue
		log.Printf("CRITICAL ERROR from run(): %v\n", err)
		currentTrace := string(debug.Stack())
		log.Printf("Trace: %s\n", currentTrace)
		// Also log with slog if it's available
		slog.Error("CRITICAL ERROR from run()", "error", err.Error(), "trace", currentTrace)
		os.Exit(1)
	}
}

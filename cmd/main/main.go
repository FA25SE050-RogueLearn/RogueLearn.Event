package main

import (
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"runtime/debug"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/cmd/api"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/database"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/protos"
	"google.golang.org/grpc"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/client/executor"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/client/rabbitmq"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/handlers"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/service"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/env"
	"github.com/joho/godotenv"
	"github.com/lmittmann/tint"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Printf("Error loading .env file: %v", err)
	}

	cfg := &api.Config{
		HttpPort: 8080,
		GrpcPort: 8081,
	}

	// test area
	connStr := env.GetString("SUPABASE_DB_CONNECTION_STRING", "")
	if connStr == "" {
		panic("SUPABASE_DB_CONNECTION_STRING environment variable is not set")
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

	rabbitMQURL := env.GetString("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	rabbitClient, err := rabbitmq.NewRabbitMQClient(rabbitMQURL, logger)
	if err != nil {
		panic(fmt.Sprintf("Could not connect to RabbitMQ: %v", err))
	}
	defer rabbitClient.Close()

	executorURL := env.GetString("EXECUTOR_URL", "")
	if executorURL == "" {
		panic("EXECUTOR_URL environment variable is not set")
	}
	executorClient, err := executor.NewClient(executorURL)
	if err != nil {
		panic(fmt.Sprintf("Could not connect to Executor: %v", err))
	}
	defer executorClient.Close()

	handlerRepo := handlers.NewHandlerRepo(logger, db, queries, rabbitClient, executorClient)

	app := api.NewApplication(cfg, logger, queries, handlerRepo)

	// run grpc server
	lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", cfg.GrpcPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	protos.RegisterCodeBattleServiceServer(grpcServer, service.NewCodeBattleServer(queries, logger))

	go grpcServer.Serve(lis)

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

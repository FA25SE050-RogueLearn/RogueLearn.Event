package api

import (
	"log/slog"
	"sync"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/handlers"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
)

type Application struct {
	wg       sync.WaitGroup
	cfg      *Config
	handlers *handlers.HandlerRepo
	logger   *slog.Logger
	queries  *store.Queries
}

func NewApplication(cfg *Config, logger *slog.Logger, queries *store.Queries, handlerRepo *handlers.HandlerRepo) *Application {
	return &Application{
		cfg:      cfg,
		logger:   logger,
		queries:  queries,
		handlers: handlerRepo,
	}
}

type Config struct {
	HttpPort int
	GrpcPort int
}

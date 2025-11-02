package handlers

import (
	"log/slog"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/client/executor"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/client/rabbitmq"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/hub"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/env"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/jwt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HandlerRepo holds all the dependencies required by the handlers.
// This includes the application logger, services like the RoomManager,
// and the centralized store for data access.
type HandlerRepo struct {
	eventHub       *hub.EventHub
	logger         *slog.Logger
	queries        *store.Queries
	db             *pgxpool.Pool
	jwtParser      *jwt.JWTParser
	rabbitClient   *rabbitmq.RabbitMQClient
	executorClient *executor.Client
}

// NewHandlerRepo creates a new HandlerRepo with the provided dependencies.
func NewHandlerRepo(logger *slog.Logger, db *pgxpool.Pool, queries *store.Queries, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client) *HandlerRepo {
	secKey := env.GetString("JWT_SECRET_KEY", "")
	if secKey == "" {
		panic("JWT_SECRET_KEY env not found")
	}
	return &HandlerRepo{
		logger:         logger,
		db:             db,
		queries:        queries,
		jwtParser:      jwt.NewJWTParser(secKey, logger),
		eventHub:       hub.NewEventHub(queries, logger),
		rabbitClient:   rabbitClient,
		executorClient: executorClient,
	}
}

func toPgtypeUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: id,
		Valid: true,
	}
}

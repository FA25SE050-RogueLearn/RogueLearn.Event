package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/executor"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/rabbitmq"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/user"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/hub"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/env"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/jwt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventConfig holds configuration for event-related settings
type EventConfig struct {
	AssignmentDelaySeconds int // Delay before event start for guild-to-room assignment
}

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
	userClient     *user.Client
	eventConfig    EventConfig
}

// NewHandlerRepo creates a new HandlerRepo with the provided dependencies.
// It also starts the background cleanup routine for inactive rooms.
func NewHandlerRepo(ctx context.Context, logger *slog.Logger, db *pgxpool.Pool, queries *store.Queries, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client, userClient *user.Client) *HandlerRepo {
	// Load JWT configuration
	secKey := env.GetString("EVENT_SUPABASE_JWT_SECRET", "")
	if secKey == "" {
		print("EVENT_SUPABASE_JWT_SECRET env not found")
		panic("EVENT_SUPABASE_JWT_SECRET env not found")
	}

	// Load Supabase Auth Authority URL for JWT validation
	// The issuer will be constructed as: {SUPABASE_AUTH_AUTHORITY_URL}/auth/v1
	authAuthorityURL := env.GetString("EVENT_SUPABASE_AUTH_AUTHORITY_URL", "")
	print("EVENT_SUPABASE_AUTH_AUTHORITY_URL:", authAuthorityURL)
	var issuer string
	if authAuthorityURL != "" {
		issuer = authAuthorityURL + "/auth/v1"
	}

	// Audience is typically "authenticated" for Supabase
	audience := "authenticated"

	// Create the event hub (pass db for transaction support)
	eventHub := hub.NewEventHub(db, queries, logger, rabbitClient, executorClient, userClient)

	// Start the cleanup routine for inactive rooms
	// Check every 5 minutes, remove rooms inactive for more than 30 minutes
	go eventHub.StartInactiveRoomCleanup(ctx, 5*time.Minute, 30*time.Minute)

	// Load event configuration from environment variables
	assignmentDelaySeconds := env.GetInt("EVENT_ASSIGNMENT_DELAY_SECONDS", 30)
	logger.Info("Event configuration loaded",
		"assignment_delay_seconds", assignmentDelaySeconds)

	return &HandlerRepo{
		logger:         logger,
		db:             db,
		queries:        queries,
		jwtParser:      jwt.NewJWTParser(secKey, issuer, audience, logger),
		eventHub:       eventHub,
		rabbitClient:   rabbitClient,
		executorClient: executorClient,
		userClient:     userClient,
		eventConfig: EventConfig{
			AssignmentDelaySeconds: assignmentDelaySeconds,
		},
	}
}

func toPgtypeUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: id,
		Valid: true,
	}
}

// Getter methods for consumer access
func (hr *HandlerRepo) GetRabbitClient() *rabbitmq.RabbitMQClient {
	return hr.rabbitClient
}

func (hr *HandlerRepo) GetLogger() *slog.Logger {
	return hr.logger
}

// PaginationParams holds the calculated pagination parameters (limit and offset)
type PaginationParams struct {
	Limit     int32
	Offset    int32
	PageIndex int32
	PageSize  int32
}

// PaginationResponse wraps paginated data with metadata
type PaginationResponse struct {
	Items      any   `json:"items"`
	TotalCount int64 `json:"total_count"`
	TotalPages int64 `json:"total_pages"`
	PageIndex  int32 `json:"page_index"`
	PageSize   int32 `json:"page_size"`
}

// createPaginationResponse creates a standardized pagination response with metadata
func createPaginationResponse(items interface{}, totalCount int64, params PaginationParams) PaginationResponse {
	totalPages := int64(0)
	if params.PageSize > 0 {
		totalPages = (totalCount + int64(params.PageSize) - 1) / int64(params.PageSize)
	}

	return PaginationResponse{
		Items:      items,
		TotalCount: totalCount,
		TotalPages: totalPages,
		PageIndex:  params.PageIndex,
		PageSize:   params.PageSize,
	}
}

// parsePaginationParams extracts pagination parameters from the request query string
// Accepts page_size and page_index, automatically calculates limit and offset
// Default values: page_size=10, page_index=1
// Maximum page_size: 100
// page_index is 1-based (first page is 1, not 0)
func parsePaginationParams(r *http.Request) PaginationParams {
	const (
		defaultPageSize  = 10
		maxPageSize      = 100
		defaultPageIndex = 1
	)

	pageSize := defaultPageSize
	pageIndex := defaultPageIndex

	// Parse page_size from query parameter
	if pageSizeStr := r.URL.Query().Get("page_size"); pageSizeStr != "" {
		if parsedPageSize, err := strconv.Atoi(pageSizeStr); err == nil && parsedPageSize > 0 {
			pageSize = parsedPageSize
			// Cap at max page size
			if pageSize > maxPageSize {
				pageSize = maxPageSize
			}
		}
	}

	// Parse page_index from query parameter (1-based)
	if pageIndexStr := r.URL.Query().Get("page_index"); pageIndexStr != "" {
		if parsedPageIndex, err := strconv.Atoi(pageIndexStr); err == nil && parsedPageIndex >= 1 {
			pageIndex = parsedPageIndex
		}
	}

	// Calculate limit and offset
	limit := pageSize
	offset := (pageIndex - 1) * pageSize

	return PaginationParams{
		Limit:     int32(limit),
		Offset:    int32(offset),
		PageIndex: int32(pageIndex),
		PageSize:  int32(pageSize),
	}
}

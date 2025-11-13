package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
)

func (app *Application) routes() http.Handler {
	mux := chi.NewRouter()

	// Configure CORS with security in mind
	// In production, you should configure allowed origins via environment variables
	corsHandler := cors.New(cors.Options{
		AllowedOrigins: []string{},
		AllowedMethods: []string{
			"GET",
			"POST",
			"PUT",
			"PATCH",
			"DELETE",
			"OPTIONS",
		},
		AllowedHeaders: []string{},
		ExposedHeaders: []string{
			"Link",
		},
		AllowCredentials: true,
		MaxAge:           300, // 5 minutes
	})
	mux.Use(corsHandler.Handler)

	mux.Route("/events", func(r chi.Router) {
		// Public routes for events
		r.Get("/", app.handlers.GetEventsHandler)
		r.Get("/{event_id}/rooms", app.handlers.GetEventRoomsHandler)

		// Auth-protected routes for event interaction
		r.Group(func(r chi.Router) {
			r.Use(app.handlers.AuthMiddleware)
			r.Post("/{event_id}/guilds/{guild_id}/register", app.handlers.RegisterGuildToEventHandler)
			r.Get("/{event_id}/problems", app.handlers.GetEventProblemsHandler)
			r.Get("/{event_id}/leaderboard", app.handlers.SpectateEventHandler)
			r.Get("/{event_id}/rooms/{room_id}/leaderboard", app.handlers.JoinRoomHandler)
			r.Post("/{event_id}/rooms/{room_id}/submit", app.handlers.SubmitInRoomHandler)
		})
	})

	// Routes for managing event creation requests
	mux.Route("/event-requests", func(r chi.Router) {
		r.Use(app.handlers.AuthMiddleware) // All event request routes require auth

		// Requester: Submit a new request to create an event
		r.Post("/", app.handlers.CreateEventHandler)

		// Requester: View their own submitted requests
		r.Get("/my", app.handlers.GetMyEventRequestsHandler)
	})

	// Admin routes - require authentication AND "Game Master" role
	mux.Route("/admin", func(r chi.Router) {
		r.Use(app.handlers.AuthMiddleware)
		r.Use(app.handlers.AdminOnly)

		r.Get("/event-requests", app.handlers.GetEventRequestsHandler)
		r.Post("/event-requests/{request_id}/process", app.handlers.ProcessEventRequestHandler)
	})

	// Submission routes - require authentication
	mux.Route("/submissions", func(r chi.Router) {
		r.Use(app.handlers.AuthMiddleware)
		r.Post("/", app.handlers.SubmitHandler)
		r.Get("/me", app.handlers.GetMySubmissionsHandler)
	})

	mux.Route("/problems", func(r chi.Router) {
		// Public routes for problems
		r.Get("/", app.handlers.GetProblemsHandler)
		r.Get("/{problem_id}", app.handlers.GetProblemHandler)

		// Auth-protected routes for problem details
		r.Group(func(r chi.Router) {
			r.Use(app.handlers.AuthMiddleware)
			r.Get("/{problem_id}/details", app.handlers.GetProblemDetails)
		})
	})

	mux.Route("/tags", func(r chi.Router) {
		r.Get("/", app.handlers.GetTagsHandler)
	})

	mux.Route("/health", func(r chi.Router) {
		r.Get("/", app.handlers.HealthcheckHandler)
	})

	// Internal/system routes for scheduled tasks
	// This should be called by your Alpine cron service
	mux.Route("/internal", func(r chi.Router) {
		r.Get("/start-pending-events", app.handlers.StartPendingEventsHandler)
	})

	return mux
}

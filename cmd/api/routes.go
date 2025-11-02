package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
)

func (app *Application) routes() http.Handler {
	mux := chi.NewRouter()

	mux.Use(cors.AllowAll().Handler)

	mux.Route("/events", func(r chi.Router) {
		// Public routes for events
		r.Get("/", app.handlers.GetEventsHandler)
		r.Get("/{event_id}/rooms", app.handlers.GetEventRoomsHandler)

		// Auth-protected routes for event interaction
		r.Get("/{event_id}/leaderboard", app.handlers.SpectateEventHandler)
		r.Get("/{event_id}/rooms/{room_id}/leaderboard", app.handlers.JoinRoomHandler)
		r.Post("/{event_id}/rooms/{room_id}/submit", app.handlers.SubmitSolutionInRoomHandler)
		r.Get("/{event_id}/rooms/{room_id}/problems", app.handlers.GetRoomProblemsHandler)
	})

	// Routes for managing event creation requests
	mux.Route("/event-requests", func(r chi.Router) {
		// Requester: Submit a new request to create an event
		r.Post("/", app.handlers.CreateEventHandler)

		// Requester: View their own submitted requests
		r.Get("/my", app.handlers.GetMyEventRequestsHandler) // Assumes guild_id is a query param
	})

	mux.Route("/admin", func(r chi.Router) {
		r.Get("/event-requests", app.handlers.GetEventRequestsHandler)
		r.Post("/event-requests/{request_id}/process", app.handlers.ProcessEventRequestHandler)
	})

	mux.Route("/submissions", func(r chi.Router) {
		r.Post("/", app.handlers.SubmitSolutionHandler)
	})

	mux.Route("/problems", func(r chi.Router) {
		// Public routes for problems
		r.Get("/", app.handlers.GetProblemsHandler)
		r.Get("/{problem_id}", app.handlers.GetProblemHandler)

		// Auth-protected routes for problem details
		r.Get("/{problem_id}/details", app.handlers.GetProblemDetails)
	})

	mux.Route("/tags", func(r chi.Router) {
		r.Get("/", app.handlers.GetTagsHandler)
	})
	return mux
}

package handlers

import (
	"errors"
	"net/http"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/response"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type UserLeaderboardResponse struct {
	Leaderboard []UserLeaderboardEntry `json:"leaderboard"`
}

type UserLeaderboardEntry struct {
	UserID       string `json:"user_id"`
	Username     string `json:"username"`
	Rank         int32  `json:"rank"`
	Score        int32  `json:"score"`
	SnapshotDate string `json:"snapshot_date"`
}

type GuildLeaderboardResponse struct {
	Leaderboard []GuildLeaderboardEntry `json:"leaderboard"`
}

type GuildLeaderboardEntry struct {
	GuildID      string `json:"guild_id"`
	GuildName    string `json:"guild_name"`
	Rank         int32  `json:"rank"`
	TotalScore   int32  `json:"total_score"`
	SnapshotDate string `json:"snapshot_date"`
}

func (hr *HandlerRepo) GetEventLeaderboardHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, err)
		return
	}

	leaderboardType := r.URL.Query().Get("type")
	if leaderboardType == "" {
		leaderboardType = "user" // default to user leaderboard
	}

	switch leaderboardType {
	case "user":
		hr.getUserLeaderboard(w, r, eventID)
	case "guild":
		hr.getGuildLeaderboard(w, r, eventID)
	default:
		hr.badRequest(w, r, errors.New("invalid leaderboard type: must be 'user' or 'guild'"))
	}
}

func (hr *HandlerRepo) getUserLeaderboard(w http.ResponseWriter, r *http.Request, eventID uuid.UUID) {
	leaderboard, err := hr.queries.GetLeaderboardByEvent(r.Context(), toPgtypeUUID(eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		hr.logger.Error("No leaderboard data found")
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("Failed to fetch leaderboard data", "err", err)
		hr.serverError(w, r, err)
		return
	}

	entries := make([]UserLeaderboardEntry, 0, len(leaderboard))
	for _, entry := range leaderboard {
		entries = append(entries, UserLeaderboardEntry{
			UserID:       entry.UserID.String(),
			Username:     entry.Username,
			Rank:         entry.Rank,
			Score:        entry.Score,
			SnapshotDate: entry.SnapshotDate.Time.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	response.JSON(w, response.JSONResponseParameters{
		Status: http.StatusOK,
		Data: UserLeaderboardResponse{
			Leaderboard: entries,
		},
	})
}

func (hr *HandlerRepo) getGuildLeaderboard(w http.ResponseWriter, r *http.Request, eventID uuid.UUID) {
	leaderboard, err := hr.queries.GetGuildLeaderboardByEvent(r.Context(), toPgtypeUUID(eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		hr.logger.Debug("No leaderboard found")
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("Failed to fetch leaderboard data", "err", err)
		hr.serverError(w, r, err)
		return
	}

	entries := make([]GuildLeaderboardEntry, 0, len(leaderboard))
	for _, entry := range leaderboard {
		entries = append(entries, GuildLeaderboardEntry{
			GuildID:      entry.GuildID.String(),
			GuildName:    entry.GuildName,
			Rank:         entry.Rank,
			TotalScore:   entry.TotalScore,
			SnapshotDate: entry.SnapshotDate.Time.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	response.JSON(w, response.JSONResponseParameters{
		Status: http.StatusOK,
		Data: GuildLeaderboardResponse{
			Leaderboard: entries,
		},
	})
}

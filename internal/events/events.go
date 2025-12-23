package events

import (
	"time"

	"github.com/google/uuid"
)

type EventType string

const (
	CORRECT_SOLUTION_SUBMITTED EventType = "CORRECT_SOLUTION_SUBMITTED"
	WRONG_SOLUTION_SUBMITTED   EventType = "WRONG_SOLUTION_SUBMITTED"
	SOLUTION_SUBMITTED         EventType = "SOLUTION_SUBMITTED"
	PLAYER_JOINED              EventType = "PLAYER_JOINED"
	PLAYER_LEFT                EventType = "PLAYER_LEFT"
	ROOM_DELETED               EventType = "ROOM_DELETED"
	EVENT_ENDED                EventType = "EVENT_ENDED"
	COMPILATION_TEST           EventType = "COMPILATION_TEST"
	LEADERBOARD_UPDATED        EventType = "LEADERBOARD_UPDATED"
	GUILD_LEADERBOARD_UPDATED  EventType = "GUILD_LEADERBOARD_UPDATED"
	EVENT_EXPIRED              EventType = "EVENT_EXPIRED"
)

// Event wrapper for the listener
type SseEvent struct {
	EventType EventType
	Data      any
}

type JudgeStatus string

const (
	Accepted            JudgeStatus = "Accepted"
	WrongAnswer         JudgeStatus = "Wrong Answer"
	RuntimeError        JudgeStatus = "Runtime Error"
	CompilationError    JudgeStatus = "Compilation Error"
	TimeLimitExceeded   JudgeStatus = "Time Limit Exceeded"   // For the future
	MemoryLimitExceeded JudgeStatus = "Memory Limit Exceeded" // For the future
)

type SolutionSubmitted struct {
	SubmissionID  uuid.UUID
	PlayerID      uuid.UUID
	EventID       uuid.UUID
	RoomID        uuid.UUID
	ProblemID     uuid.UUID
	Code          string
	Language      string
	SubmittedTime time.Time
}

type SolutionResult struct {
	SolutionSubmitted SolutionSubmitted
	Status            JudgeStatus
	Message           string
	Score             int32
	ExecutionTimeMs   string
}

type LeaderboardUpdated struct {
	RoomID uuid.UUID
}

type PlayerJoined struct {
	PlayerID      uuid.UUID
	PlayerName    string
	PlayerGuildID uuid.UUID
	RoomID        uuid.UUID
	EventID       uuid.UUID
	EventEndTime  time.Time
}

type PlayerLeft struct {
	PlayerID uuid.UUID
	RoomID   uuid.UUID
}

type RoomDeleted struct {
	RoomID uuid.UUID
}

// EventExpired is sent to all players when an event ends.
// P2-1: Enhanced with winners, statistics, and final results
type EventExpired struct {
	EventID     uuid.UUID `json:"event_id"`
	CompletedAt time.Time `json:"completed_at"`
	Message     string    `json:"message"`

	// P2-1: Top performers
	TopPlayers []TopPlayer `json:"top_players,omitempty"`
	TopGuilds  []TopGuild  `json:"top_guilds,omitempty"`
}

// TopPlayer represents a top-ranked player in the event
type TopPlayer struct {
	Rank     int       `json:"rank"`
	UserID   uuid.UUID `json:"user_id"`
	Username string    `json:"username"`
	Score    int       `json:"score"`
	RoomID   uuid.UUID `json:"room_id,omitempty"`
}

// TopGuild represents a top-ranked guild in the event
type TopGuild struct {
	Rank       int       `json:"rank"`
	GuildID    uuid.UUID `json:"guild_id"`
	GuildName  string    `json:"guild_name"`
	TotalScore int       `json:"total_score"`
}

// EventStatistics contains aggregate statistics about the event
type EventStatistics struct {
	TotalParticipants   int     `json:"total_participants"`
	TotalSubmissions    int     `json:"total_submissions"`
	AcceptedSubmissions int     `json:"accepted_submissions"`
	AcceptanceRate      float64 `json:"acceptance_rate"`
	AverageScore        float64 `json:"average_score"`
	HighestScore        int     `json:"highest_score"`
	TotalRooms          int     `json:"total_rooms"`
}

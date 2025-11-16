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
	PlayerID uuid.UUID
	RoomID   uuid.UUID
	EventID  uuid.UUID
}

type PlayerLeft struct {
	PlayerID uuid.UUID
	RoomID   uuid.UUID
}

type RoomDeleted struct {
	RoomID uuid.UUID
}

type EventExpired struct {
	EventID     uuid.UUID `json:"event_id"`
	CompletedAt time.Time `json:"completed_at"`
	Message     string    `json:"message"`
}

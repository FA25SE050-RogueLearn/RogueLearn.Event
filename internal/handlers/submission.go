package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/events"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/request"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/response"
	pb "github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/protos"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DefaultQueryTimeoutSecond = 15 * time.Second
)

var (
	ErrLanguageNotFound error = errors.New("Invalid programming language")
	ErrInvalidProblem   error = errors.New("Invalid problem")
	ErrWrongSyntax      error = errors.New("Wrong syntax")
)

type SubmissionRequest struct {
	ProblemID string `json:"problem_id"`
	Code      string `json:"code"`
	Language  string `json:"language"`
}

type SubmissionResponse struct {
	Stdout          string                 `json:"stdout,omitempty"`
	Stderr          string                 `json:"stderr,omitempty"`
	Message         string                 `json:"message,omitempty"`
	Error           string                 `json:"error,omitempty"`
	Success         bool                   `json:"success,omitempty"`
	CodeProblemID   string                 `json:"code_problem_id,omitempty"`
	ProblemTitle    string                 `json:"problem_title,omitempty"`
	LanguageID      string                 `json:"language_id,omitempty"`
	LanguageName    string                 `json:"language_name,omitempty"`
	Status          store.SubmissionStatus `json:"status"`
	SubmittedAt     time.Time              `json:"submitted_at"`
	ExecutionTimeMs string                 `json:"execution_time_ms,omitempty"`
}

// SubmitSolutionHandler will compile and run test cases of a solution for a code problem
func (hr *HandlerRepo) SubmitHandler(w http.ResponseWriter, r *http.Request) {
	var req SubmissionRequest
	err := request.DecodeJSON(w, r, &req)
	if err != nil {
		hr.badRequest(w, r, ErrInvalidRequest)
		return
	}

	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.logger.Error("failed to get user claims", "err", err)
		hr.badRequest(w, r, err)
		return
	}

	hr.logger.Info("userClaims parsed!", "user_claims", *userClaims)

	// get player_id through query param in development stage
	playerID, err := uuid.Parse(userClaims.GetUserID())
	if err != nil {
		hr.badRequest(w, r, errors.New("failed to parse player_id"))
		return
	}

	problemIDUID, err := uuid.Parse(req.ProblemID)
	if err != nil {
		hr.badRequest(w, r, ErrInvalidProblem)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeoutSecond)
	defer cancel()

	lang, err := hr.queries.GetLanguageByName(ctx, req.Language)
	if err != nil {
		hr.logger.Error("failed to get language",
			"lang", req.Language,
			"err", err)
		return
	}

	problem, err := hr.queries.GetCodeProblemLanguageDetail(ctx, store.GetCodeProblemLanguageDetailParams{
		CodeProblemID: toPgtypeUUID(problemIDUID),
		LanguageID:    lang.ID,
	})
	if err != nil {
		hr.logger.Error("Error", "question", err)
		hr.badRequest(w, r, ErrInternalServer)
		return
	}

	testCases, err := hr.queries.GetTestCasesByProblem(ctx, problem.CodeProblemID)
	if err != nil {
		hr.logger.Error("failed to get test cases", "problem_id", problem.CodeProblemID)
		hr.badRequest(w, r, ErrInternalServer)
		return
	}

	s, err := hr.queries.CreateSubmission(r.Context(), store.CreateSubmissionParams{
		UserID:        toPgtypeUUID(playerID),
		LanguageID:    lang.ID,
		CodeProblemID: toPgtypeUUID(problemIDUID),
		CodeSubmitted: req.Code,
		Status:        store.SubmissionStatusPending,
	})
	if err != nil {
		hr.logger.Error("failed to create submission", "err", err)
		hr.serverError(w, r, ErrInternalServer)
		return
	}

	// Convert test cases from store.TestCase to pb.TestCase
	pbTestCases := convertTestCases(testCases)

	// Send grpc request to executor service
	request := pb.ExecuteRequest{
		Language:     lang.Name,
		Code:         req.Code,
		DriverCode:   problem.DriverCode,
		CompileCmd:   lang.CompileCmd,
		RunCmd:       lang.RunCmd,
		TempFileDir:  lang.TempFileDir.String,
		TempFileName: lang.TempFileName.String,
		TestCases:    pbTestCases,
	}

	hr.logger.Info("Sending execution request...", "request", request)

	result, err := hr.executorClient.ExecuteCode(r.Context(), &request)
	if err != nil {
		hr.logger.Error("failed to execute code", "err", err)
		hr.serverError(w, r, ErrInternalServer)
		return
	}

	err = hr.updateSubmission(ctx, s.ID, result)
	if err != nil {
		hr.logger.Error("failed to update submission status", "err", err)
		hr.serverError(w, r, ErrInternalServer)
		return
	}

	response.JSON(w, response.JSONResponseParameters{
		Status: http.StatusOK,
		Data: SubmissionResponse{
			Stdout:          result.Stdout,
			Stderr:          result.Stderr,
			Message:         result.Message,
			Success:         result.Success,
			Error:           result.ErrorType,
			CodeProblemID:   s.CodeProblemID.String(),
			LanguageID:      s.LanguageID.String(),
			Status:          s.Status,
			SubmittedAt:     s.SubmittedAt.Time,
			ExecutionTimeMs: result.ExecutionTimeMs,
		},
		Success: true,
		Msg:     "submitted successfully.",
		ErrMsg:  "",
	})
}

func (hr *HandlerRepo) GetMySubmissionsHandler(w http.ResponseWriter, r *http.Request) {
	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.logger.Error("failed to get user claims", "err", err)
		hr.badRequest(w, r, err)
		return
	}

	hr.logger.Info("userClaims parsed!", "user_claims", *userClaims)

	// get player_id through query param in development stage
	userID, err := uuid.Parse(userClaims.GetUserID())
	if err != nil {
		hr.badRequest(w, r, errors.New("failed to parse player_id"))
		return
	}

	submissions, err := hr.queries.GetSubmissionsByUser(r.Context(), toPgtypeUUID(userID))
	if err != nil {
		hr.logger.Error("failed to get submissions", "err", err)
		hr.serverError(w, r, err)
		return
	}

	response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    hr.toSubmissionResponses(submissions),
		Success: true,
		Msg:     "get submissions successfully",
	})
}

// Helper function to convert execution time from string (e.g., "200ms") to pgtype.Int4
func parseExecutionTime(execTimeStr string) pgtype.Int4 {
	if execTimeStr == "" {
		return pgtype.Int4{}
	}

	// Remove "ms" suffix and parse the numeric value
	timeStr := strings.TrimSuffix(execTimeStr, "ms")
	timeValue, err := strconv.ParseInt(timeStr, 10, 32)
	if err != nil {
		// Return null pgtype.Int4 if parsing fails
		return pgtype.Int4{}
	}

	return pgtype.Int4{Int32: int32(timeValue), Valid: true}
}

// Helper function to convert pgtype.Int4 to string with "ms" suffix
func formatExecutionTime(execTime pgtype.Int4) string {
	if !execTime.Valid {
		return ""
	}
	return strconv.FormatInt(int64(execTime.Int32), 10) + "ms"
}

// toSubmissionResponses converts database submission rows to response format
func (hr *HandlerRepo) toSubmissionResponses(submissions []store.GetSubmissionsByUserRow) []SubmissionResponse {
	var responses []SubmissionResponse
	for _, s := range submissions {
		responses = append(responses, SubmissionResponse{
			CodeProblemID:   s.CodeProblemID.String(),
			ProblemTitle:    s.ProblemTitle,
			LanguageName:    s.LanguageName,
			Status:          s.Status,
			SubmittedAt:     s.SubmittedAt.Time,
			ExecutionTimeMs: formatExecutionTime(s.ExecutionTimeMs),
		})
	}
	return responses
}

func (hr *HandlerRepo) updateSubmission(ctx context.Context, submissionID pgtype.UUID, result *pb.ExecuteResponse) error {
	var status store.SubmissionStatus

	if result.Success {
		status = store.SubmissionStatusAccepted
	} else {
		// Use a switch on the error type from the ExecuteResponse
		switch result.ErrorType {
		case "RUNTIME_ERROR":
			status = store.SubmissionStatusRuntimeError
		case "COMPILE_ERROR":
			status = store.SubmissionStatusCompilationError
		case "WRONG_ANSWER":
			status = store.SubmissionStatusWrongAnswer
		default:
			// Add a default case to catch nil or unknown errors.
			// This prevents the zero-value bug.
			status = store.SubmissionStatusRuntimeError // A safe default for unknown failures.
		}
	}

	// Convert execution time from string (e.g., "200ms") to pgtype.Int4
	executionTimeMs := parseExecutionTime(result.ExecutionTimeMs)
	if executionTimeMs == (pgtype.Int4{}) && result.ExecutionTimeMs != "" {
		hr.logger.Warn("failed to parse execution time", "value", result.ExecutionTimeMs)
	}

	_, err := hr.queries.UpdateSubmission(ctx, store.UpdateSubmissionParams{
		ID:              submissionID,
		Status:          status,
		ExecutionTimeMs: executionTimeMs,
	})
	if err != nil {
		return err
	}

	return nil
}

// SubmitSolutionInRoomHandler creates an event and pass it to the room's channel
func (hr *HandlerRepo) SubmitInRoomHandler(w http.ResponseWriter, r *http.Request) {
	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.logger.Error("failed to get user claims", "err", err)
		hr.badRequest(w, r, err)
		return
	}

	hr.logger.Info("userClaims parsed!", "user_claims", *userClaims)

	// get player_id through query param in development stage
	playerID, err := uuid.Parse(userClaims.GetUserID())
	if err != nil {
		hr.badRequest(w, r, errors.New("failed to parse player_id"))
		return
	}

	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID in URL"))
		return
	}

	roomIDStr := chi.URLParam(r, "room_id")
	roomID, err := uuid.Parse(roomIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid room ID in URL"))
		return
	}

	var reqPayload SubmissionRequest
	err = request.DecodeJSON(w, r, &reqPayload)
	if err != nil {
		hr.badRequest(w, r, err)
		return
	}

	problemID, err := uuid.Parse(reqPayload.ProblemID)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid problem ID in request body"))
		return
	}

	// CRITICAL: Check if player has already solved this problem
	// Prevent duplicate submissions and point farming
	existingSolution, err := hr.queries.CheckIfProblemAlreadySolved(r.Context(), store.CheckIfProblemAlreadySolvedParams{
		UserID:        pgtype.UUID{Bytes: playerID, Valid: true},
		CodeProblemID: pgtype.UUID{Bytes: problemID, Valid: true},
		RoomID:        pgtype.UUID{Bytes: roomID, Valid: true},
	})

	if err == nil {
		// Problem already solved - return error
		hr.logger.Info("Player attempted to resubmit already solved problem",
			"player_id", playerID,
			"problem_id", problemID,
			"room_id", roomID,
			"original_submission_id", existingSolution.ID,
			"solved_at", existingSolution.SubmittedAt)

		hr.errorMessage(w, r, http.StatusConflict, "You have already solved this problem.", nil)

		return
	}
	// If error is "no rows", that's fine - problem not solved yet, continue
	// Any other error should be logged but not block submission (fail open for availability)

	// Get or create the room hub - supports lazy-loading from database
	// This ensures submissions work even if the room wasn't pre-loaded on this instance
	roomHub := hr.eventHub.GetOrCreateRoomHub(r.Context(), roomID)
	if roomHub == nil {
		hr.notFound(w, r)
		return
	}

	submissionEvent := events.SolutionSubmitted{
		PlayerID:      playerID,
		EventID:       eventID,
		RoomID:        roomID,
		ProblemID:     problemID,
		Code:          reqPayload.Code,
		Language:      reqPayload.Language,
		SubmittedTime: time.Now(),
	}

	// Use a select with a default case to avoid blocking if the channel is full
	select {
	case roomHub.Events <- submissionEvent:
		// Event sent successfully
	default:
		hr.logger.Warn("event hub channel is full, submission dropped", "room_id", roomID)
		hr.errorMessage(w, r, http.StatusServiceUnavailable, "Server is busy, please try again later.", nil)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusAccepted,
		Success: true,
		Msg:     "Submitted successfully and is being processed.",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

// Helper function to convert store.TestCase to pb.TestCase
func convertTestCases(storeTestCases []store.TestCase) []*pb.TestCase {
	pbTestCases := make([]*pb.TestCase, len(storeTestCases))
	for i, tc := range storeTestCases {
		pbTestCases[i] = &pb.TestCase{
			Input:          tc.Input,
			ExpectedOutput: tc.ExpectedOutput,
		}
	}
	return pbTestCases
}

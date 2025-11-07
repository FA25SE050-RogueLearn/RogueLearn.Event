package handlers

import (
	"context"
	"errors"
	"net/http"
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
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	Message       string `json:"message"`
	Success       bool   `json:"success"`
	Error         string `json:"error"`
	ExecutionTime string `json:"execution_time_ms"`
}

// SubmitSolutionHandler will compile and run test cases of a solution for a code problem
func (hr *HandlerRepo) SubmitSolutionHandler(w http.ResponseWriter, r *http.Request) {
	var req SubmissionRequest
	err := request.DecodeJSON(w, r, &req)
	if err != nil {
		hr.badRequest(w, r, ErrInvalidRequest)
		return
	}

	// get player_id through query param in development stage
	playerIDStr := r.URL.Query().Get("player_id")
	playerID, err := uuid.Parse(playerIDStr)
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

	err = hr.updateSubmissionStatus(ctx, s.ID, result)
	if err != nil {
		hr.logger.Error("failed to update submission status", "err", err)
		hr.serverError(w, r, ErrInternalServer)
		return
	}

	response.JSON(w, response.JSONResponseParameters{
		Status: http.StatusOK,
		Data: SubmissionResponse{
			Stdout:        result.Stdout,
			Stderr:        result.Stderr,
			Message:       result.Message,
			Success:       result.Success,
			Error:         result.ErrorType,
			ExecutionTime: result.ExecutionTimeMs,
		},
		Success: true,
		Msg:     "solution submitted successfully.",
		ErrMsg:  "",
	})
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

func (hr *HandlerRepo) updateSubmissionStatus(ctx context.Context, submissionID pgtype.UUID, result *pb.ExecuteResponse) error {
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

	_, err := hr.queries.UpdateSubmissionStatus(ctx, store.UpdateSubmissionStatusParams{
		ID:     submissionID,
		Status: status,
	})
	if err != nil {
		return err
	}

	return nil
}

// SubmitSolutionInRoomHandler creates an event and pass it to the room's channel
func (hr *HandlerRepo) SubmitSolutionInRoomHandler(w http.ResponseWriter, r *http.Request) {
	// claims, ok := r.Context().Value("asd").(*jwt.UserClaims)
	// if !ok {
	// 	// hr.unauthorizedResponse(w, r)
	// 	return
	// }

	// get player_id through query param on dev stage.
	playerIDStr := r.URL.Query().Get("player_id")
	playerID, err := uuid.Parse(playerIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid user ID in token"))
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

	roomHub := hr.eventHub.GetRoomById(roomID)
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
		Msg:     "Solution submitted successfully and is being processed.",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

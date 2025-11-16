package handlers

import (
	"net/http"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/response"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type CodeProblemResponse struct {
	ID               uuid.UUID `json:"id"`
	Title            string    `json:"title"`
	ProblemStatement string    `json:"problem_statement"`
	Difficulty       int32     `json:"difficulty"`
}

func (hr *HandlerRepo) GetProblemsHandler(w http.ResponseWriter, r *http.Request) {
	pagination := parsePaginationParams(r)

	totalCount, err := hr.queries.CountCodeProblems(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	problems, err := hr.queries.GetCodeProblems(r.Context(), store.GetCodeProblemsParams{
		Limit:  pagination.Limit,
		Offset: pagination.Offset,
	})
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	paginatedResponse := createPaginationResponse(toProblemResponses(problems), totalCount, pagination)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    paginatedResponse,
		Success: true,
		Msg:     "Problems retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

func (hr *HandlerRepo) GetProblemHandler(w http.ResponseWriter, r *http.Request) {
	pIDStr := chi.URLParam(r, "problem_id")
	pIDUID, err := uuid.Parse(pIDStr)
	if err != nil {
		hr.badRequest(w, r, ErrInvalidRequest)
		return
	}

	problem, err := hr.queries.GetCodeProblemByID(r.Context(), toPgtypeUUID(pIDUID))
	if err == pgx.ErrNoRows {
		hr.logger.Info("problem not found", "problem_id", pIDUID)
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get code problem", "err", err)
		hr.serverError(w, r, err)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    toProblemResponse(problem),
		Success: true,
		Msg:     "Problems retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

func (hr *HandlerRepo) GetEventProblemsHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventIDUID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, err)
		return
	}

	cps, err := hr.queries.GetEventCodeProblems(r.Context(), toPgtypeUUID(eventIDUID))
	if err == pgx.ErrNoRows {
		hr.logger.Info("event's code problems not found", "event_id", eventIDUID)
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get event code problems", "err", err)
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("event code problems found", "event_code_problems", cps)

	// Convert to CodeProblemResponse slice
	problemResponses := make([]CodeProblemResponse, len(cps))
	for i, cp := range cps {
		problemResponses[i] = CodeProblemResponse{
			ID:               cp.CodeProblemID.Bytes,
			Title:            cp.Title,
			ProblemStatement: cp.ProblemStatement,
			Difficulty:       cp.Difficulty,
		}
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Success: true,
		Msg:     "get code problems successfully",
		Data:    problemResponses,
	})
	if err != nil {
		hr.logger.Error("failed to parse json", "err", err)
		hr.serverError(w, r, err)
	}
}

type CodeProblemLanguageDetailResponse struct {
	SolutionStub      string `json:"solution_stub"`
	DriverCode        string `json:"driver_code"`
	TimeConstraintMs  int32  `json:"time_constraint_ms"`
	SpaceConstraintMb int32  `json:"space_constraint_mb"`
}

func (hr *HandlerRepo) GetProblemDetails(w http.ResponseWriter, r *http.Request) {
	problemIDStr := chi.URLParam(r, "problem_id")
	problemID, err := uuid.Parse(problemIDStr)
	if err != nil {
		hr.badRequest(w, r, err)
		return
	}

	lang := r.URL.Query().Get("lang")

	detail, err := hr.queries.GetCodeProblemLanguageDetailByLanguageName(r.Context(), store.GetCodeProblemLanguageDetailByLanguageNameParams{
		CodeProblemID: toPgtypeUUID(problemID),
		Name:          lang,
	})

	if err == pgx.ErrNoRows {
		hr.logger.Error("No detail found")
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get code problem language detail", "err", err)
		hr.serverError(w, r, err)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Success: true,
		Msg:     "get code problem language detail successfully",
		Data:    toProblemDetailResponse(detail),
	})
	if err != nil {
		hr.logger.Error("failed to parse json", "err", err)
		hr.serverError(w, r, err)
	}
}

type TagResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func (hr *HandlerRepo) GetTagsHandler(w http.ResponseWriter, r *http.Request) {
	pagination := parsePaginationParams(r)

	totalCount, err := hr.queries.CountTags(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	params := store.GetTagsParams{
		Limit:  pagination.Limit,
		Offset: pagination.Offset,
	}

	tags, err := hr.queries.GetTags(r.Context(), params)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	tagResponses := make([]TagResponse, len(tags))
	for i, tag := range tags {
		tagResponses[i] = TagResponse{
			ID:   tag.ID.Bytes,
			Name: tag.Name,
		}
	}

	paginatedResponse := createPaginationResponse(tagResponses, totalCount, pagination)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    paginatedResponse,
		Success: true,
		Msg:     "Tags retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

func toProblemResponse(problem store.CodeProblem) CodeProblemResponse {
	return CodeProblemResponse{
		Title:            problem.Title,
		ProblemStatement: problem.ProblemStatement,
		Difficulty:       problem.Difficulty,
	}
}

func toProblemResponses(problems []store.CodeProblem) []CodeProblemResponse {
	responses := make([]CodeProblemResponse, len(problems))
	for i, problem := range problems {
		responses[i] = CodeProblemResponse{
			ID:               problem.ID.Bytes,
			Title:            problem.Title,
			ProblemStatement: problem.ProblemStatement,
			Difficulty:       problem.Difficulty,
		}
	}
	return responses
}

func toProblemDetailResponse(problem store.CodeProblemLanguageDetail) CodeProblemLanguageDetailResponse {
	return CodeProblemLanguageDetailResponse{
		SolutionStub:      problem.SolutionStub,
		DriverCode:        problem.DriverCode,
		TimeConstraintMs:  problem.TimeConstraintMs,
		SpaceConstraintMb: problem.SpaceConstraintMb,
	}
}

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	pb "github.com/FA25SE050-RogueLearn/RogueLearn.Event/protos"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/request"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/response"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type CodeProblemResponse struct {
	ID                 uuid.UUID `json:"id"`
	Title              string    `json:"title"`
	ProblemStatement   string    `json:"problem_statement"`
	Difficulty         int32     `json:"difficulty"`
	SupportedLanguages []string  `json:"supported_languages"`
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

	// Fetch supported languages for all problems
	problemIDs := make([]pgtype.UUID, len(problems))
	for i, p := range problems {
		problemIDs[i] = p.ID
	}

	supportedLangs, err := hr.queries.GetSupportedLanguagesForProblems(r.Context(), problemIDs)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// Build a map of problem ID to supported languages
	langMap := make(map[uuid.UUID][]string)
	for _, sl := range supportedLangs {
		pid := sl.CodeProblemID.Bytes
		langMap[pid] = append(langMap[pid], sl.LanguageName)
	}

	paginatedResponse := createPaginationResponse(toProblemResponsesWithLanguages(problems, langMap), totalCount, pagination)

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

	// Fetch supported languages for all problems
	problemIDs := make([]pgtype.UUID, len(cps))
	for i, cp := range cps {
		problemIDs[i] = cp.CodeProblemID
	}

	supportedLangs, err := hr.queries.GetSupportedLanguagesForProblems(r.Context(), problemIDs)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// Build a map of problem ID to supported languages
	langMap := make(map[uuid.UUID][]string)
	for _, sl := range supportedLangs {
		pid := sl.CodeProblemID.Bytes
		langMap[pid] = append(langMap[pid], sl.LanguageName)
	}

	// Convert to CodeProblemResponse slice
	problemResponses := make([]CodeProblemResponse, len(cps))
	for i, cp := range cps {
		pid := cp.CodeProblemID.Bytes
		problemResponses[i] = CodeProblemResponse{
			ID:                 pid,
			Title:              cp.Title,
			ProblemStatement:   cp.ProblemStatement,
			Difficulty:         cp.Difficulty,
			SupportedLanguages: langMap[pid],
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

type TagListResponse struct {
	Data []TagResponse `json:"data"`
}

type TagResponse struct {
	ID              uuid.UUID         `json:"id"`
	Name            string            `json:"name"`
	DifficultyCount []DifficultyCount `json:"difficulty_count"`
}

type DifficultyCount struct {
	Difficulty   int `json:"difficulty"`
	ProblemCount int `json:"problem_count"`
}

func (hr *HandlerRepo) GetTagsHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := hr.queries.GetTagsWithProblemCounts(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	tagResponses := toTagResponses(rows)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    tagResponses,
		Success: true,
		Msg:     "Tags retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

func toTagResponses(rows []store.GetTagsWithProblemCountsRow) []TagResponse {
	// Map to group rows by tag ID
	tagMap := make(map[uuid.UUID]*TagResponse)

	// Iterate through each row and group by tag
	for _, row := range rows {
		tagID := row.ID.Bytes

		// If we haven't seen this tag yet, create a new TagResponse
		if _, exists := tagMap[tagID]; !exists {
			tagMap[tagID] = &TagResponse{
				ID:              tagID,
				Name:            row.Name,
				DifficultyCount: make([]DifficultyCount, 0),
			}
		}

		// Add difficulty count if difficulty is not null and problem_count > 0
		if row.Difficulty.Valid && row.ProblemCount > 0 {
			tagMap[tagID].DifficultyCount = append(tagMap[tagID].DifficultyCount, DifficultyCount{
				Difficulty:   int(row.Difficulty.Int32),
				ProblemCount: int(row.ProblemCount),
			})
		}
	}

	// Convert map to slice
	responses := make([]TagResponse, 0, len(tagMap))
	for _, tag := range tagMap {
		responses = append(responses, *tag)
	}

	return responses
}

type CreateTagRequest struct {
	Name string `json:"name"`
}

type UpdateTagRequest struct {
	Name string `json:"name"`
}

type TagSimpleResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func (hr *HandlerRepo) CreateTagHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateTagRequest
	if err := request.DecodeJSON(w, r, &req); err != nil {
		hr.badRequest(w, r, ErrInvalidRequest)
		return
	}

	if req.Name == "" {
		hr.badRequest(w, r, errors.New("tag name cannot be empty"))
		return
	}

	// Check if tag with same name already exists
	_, err := hr.queries.GetTagByName(r.Context(), req.Name)
	if err == nil {
		hr.badRequest(w, r, errors.New("tag with this name already exists"))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		hr.serverError(w, r, err)
		return
	}

	tag, err := hr.queries.CreateTag(r.Context(), req.Name)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status: http.StatusCreated,
		Data: TagSimpleResponse{
			ID:   tag.ID.Bytes,
			Name: tag.Name,
		},
		Success: true,
		Msg:     "Tag created successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

func (hr *HandlerRepo) UpdateTagHandler(w http.ResponseWriter, r *http.Request) {
	tagIDStr := chi.URLParam(r, "tag_id")
	tagID, err := uuid.Parse(tagIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid tag ID"))
		return
	}

	var req UpdateTagRequest
	if err := request.DecodeJSON(w, r, &req); err != nil {
		hr.badRequest(w, r, ErrInvalidRequest)
		return
	}

	if req.Name == "" {
		hr.badRequest(w, r, errors.New("tag name cannot be empty"))
		return
	}

	// Check if tag exists
	_, err = hr.queries.GetTagByID(r.Context(), toPgtypeUUID(tagID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hr.notFound(w, r)
			return
		}
		hr.serverError(w, r, err)
		return
	}

	// Check if another tag with the same name exists
	existingTag, err := hr.queries.GetTagByName(r.Context(), req.Name)
	if err == nil && existingTag.ID.Bytes != tagID {
		hr.badRequest(w, r, errors.New("tag with this name already exists"))
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		hr.serverError(w, r, err)
		return
	}

	tag, err := hr.queries.UpdateTag(r.Context(), store.UpdateTagParams{
		ID:   toPgtypeUUID(tagID),
		Name: req.Name,
	})
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status: http.StatusOK,
		Data: TagSimpleResponse{
			ID:   tag.ID.Bytes,
			Name: tag.Name,
		},
		Success: true,
		Msg:     "Tag updated successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

func (hr *HandlerRepo) DeleteTagHandler(w http.ResponseWriter, r *http.Request) {
	tagIDStr := chi.URLParam(r, "tag_id")
	tagID, err := uuid.Parse(tagIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid tag ID"))
		return
	}

	// Check if tag exists
	_, err = hr.queries.GetTagByID(r.Context(), toPgtypeUUID(tagID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hr.notFound(w, r)
			return
		}
		hr.serverError(w, r, err)
		return
	}

	err = hr.queries.DeleteTag(r.Context(), toPgtypeUUID(tagID))
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    nil,
		Success: true,
		Msg:     "Tag deleted successfully",
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

func toProblemResponsesWithLanguages(problems []store.CodeProblem, langMap map[uuid.UUID][]string) []CodeProblemResponse {
	responses := make([]CodeProblemResponse, len(problems))
	for i, problem := range problems {
		pid := problem.ID.Bytes
		responses[i] = CodeProblemResponse{
			ID:                 pid,
			Title:              problem.Title,
			ProblemStatement:   problem.ProblemStatement,
			Difficulty:         problem.Difficulty,
			SupportedLanguages: langMap[pid],
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

type CreateProblemRequest struct {
	Title            string                       `json:"title"`
	ProblemStatement string                       `json:"problem_statement"`
	Difficulty       int32                        `json:"difficulty"`
	TagIDs           []string                     `json:"tag_ids"`
	LanguageDetails  []ProblemLanguageDetailInput `json:"language_details"`
}

type ProblemLanguageDetailInput struct {
	LanguageID        string          `json:"language_id"`
	SolutionStub      string          `json:"solution_stub"`
	DriverCode        string          `json:"driver_code"`
	TimeConstraintMs  int32           `json:"time_constraint_ms"`
	SpaceConstraintMb int32           `json:"space_constraint_mb"`
	TestCases         []TestCaseInput `json:"test_cases"`
	SolutionCode      string          `json:"solution_code"` // Test solution for this language
}

type TestCaseInput struct {
	Input          string `json:"input"`
	ExpectedOutput string `json:"expected_output"`
	IsHidden       bool   `json:"is_hidden"`
}

type CreateProblemResponse struct {
	ProblemID   string                     `json:"problem_id"`
	Title       string                     `json:"title"`
	TestResults *ExecutionValidationResult `json:"test_results"`
}

type ExecutionValidationResult struct {
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	PassedTests  int    `json:"passed_tests"`
	TotalTests   int    `json:"total_tests"`
	ExecutionLog string `json:"execution_log,omitempty"`
}

// CreateProblemHandler creates a new code problem with language details and test cases
// This handler is for admin use only
func (hr *HandlerRepo) CreateProblemHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateProblemRequest
	if err := request.DecodeJSON(w, r, &req); err != nil {
		hr.badRequest(w, r, ErrInvalidRequest)
		return
	}

	// Validate request
	if err := validateCreateProblemRequest(&req); err != nil {
		hr.badRequest(w, r, err)
		return
	}

	// Validate test solutions for all languages before creating the problem
	validationResults, err := hr.validateAllTestSolutions(r.Context(), &req)
	if err != nil {
		hr.logger.Error("failed to validate test solutions", "err", err)
		hr.serverError(w, r, errors.New("failed to validate test solutions"))
		return
	}

	// Check if all validations passed
	allPassed := true
	for _, result := range validationResults {
		if !result.Success {
			allPassed = false
			break
		}
	}

	if !allPassed {
		hr.logger.Warn("test solution validation failed for one or more languages")
		err = response.JSON(w, response.JSONResponseParameters{
			Status:  http.StatusBadRequest,
			Success: false,
			Msg:     "Test solution validation failed for one or more languages",
			Data: map[string]interface{}{
				"validation_results": validationResults,
			},
		})
		if err != nil {
			hr.serverError(w, r, err)
		}
		return
	}

	// Start transaction to create problem with all its details
	tx, err := hr.db.Begin(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := hr.queries.WithTx(tx)

	// Create the code problem
	problem, err := qtx.CreateCodeProblem(r.Context(), store.CreateCodeProblemParams{
		Title:            req.Title,
		ProblemStatement: req.ProblemStatement,
		Difficulty:       req.Difficulty,
	})
	if err != nil {
		hr.logger.Error("failed to create code problem", "err", err)
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("created code problem", "problem_id", problem.ID.Bytes)

	// Create problem tags
	for _, tagIDStr := range req.TagIDs {
		tagID, err := uuid.Parse(tagIDStr)
		if err != nil {
			hr.badRequest(w, r, fmt.Errorf("invalid tag_id: %s", tagIDStr))
			return
		}

		err = qtx.CreateCodeProblemTag(r.Context(), store.CreateCodeProblemTagParams{
			CodeProblemID: problem.ID,
			TagID:         toPgtypeUUID(tagID),
		})
		if err != nil {
			hr.logger.Error("failed to create problem tag", "err", err, "tag_id", tagID)
			hr.serverError(w, r, err)
			return
		}
	}

	// Create language details and test cases for each language
	for _, langDetail := range req.LanguageDetails {
		langID, err := uuid.Parse(langDetail.LanguageID)
		if err != nil {
			hr.badRequest(w, r, fmt.Errorf("invalid language_id: %s", langDetail.LanguageID))
			return
		}

		// Create language detail
		_, err = qtx.CreateCodeProblemLanguageDetail(r.Context(), store.CreateCodeProblemLanguageDetailParams{
			CodeProblemID:     problem.ID,
			LanguageID:        toPgtypeUUID(langID),
			SolutionStub:      langDetail.SolutionStub,
			DriverCode:        langDetail.DriverCode,
			TimeConstraintMs:  langDetail.TimeConstraintMs,
			SpaceConstraintMb: langDetail.SpaceConstraintMb,
		})
		if err != nil {
			hr.logger.Error("failed to create language detail", "err", err, "language_id", langID)
			hr.serverError(w, r, err)
			return
		}

		// Create test cases for this language
		for _, testCase := range langDetail.TestCases {
			_, err = qtx.CreateTestCase(r.Context(), store.CreateTestCaseParams{
				CodeProblemID:  problem.ID,
				Input:          testCase.Input,
				ExpectedOutput: testCase.ExpectedOutput,
				IsHidden:       testCase.IsHidden,
			})
			if err != nil {
				hr.logger.Error("failed to create test case", "err", err)
				hr.serverError(w, r, err)
				return
			}
		}

		hr.logger.Info("created language details and test cases",
			"problem_id", problem.ID.Bytes,
			"language_id", langID,
			"test_cases_count", len(langDetail.TestCases))
	}

	// Commit transaction
	if err = tx.Commit(r.Context()); err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("successfully created code problem with all details",
		"problem_id", problem.ID.Bytes,
		"title", problem.Title)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusCreated,
		Success: true,
		Msg:     "Code problem created successfully",
		Data: map[string]interface{}{
			"problem_id":         uuid.UUID(problem.ID.Bytes).String(),
			"title":              problem.Title,
			"validation_results": validationResults,
		},
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

type UpdateProblemRequest struct {
	Title            string   `json:"title"`
	ProblemStatement string   `json:"problem_statement"`
	Difficulty       int32    `json:"difficulty"`
	TagIDs           []string `json:"tag_ids"`
}

// UpdateProblemHandler updates an existing code problem's basic information
// This handler is for admin use only
func (hr *HandlerRepo) UpdateProblemHandler(w http.ResponseWriter, r *http.Request) {
	problemIDStr := chi.URLParam(r, "problem_id")
	problemID, err := uuid.Parse(problemIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid problem ID"))
		return
	}

	var req UpdateProblemRequest
	if err := request.DecodeJSON(w, r, &req); err != nil {
		hr.badRequest(w, r, ErrInvalidRequest)
		return
	}

	// Validate request
	if err := validateUpdateProblemRequest(&req); err != nil {
		hr.badRequest(w, r, err)
		return
	}

	// Check if problem exists
	_, err = hr.queries.GetCodeProblemByID(r.Context(), toPgtypeUUID(problemID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hr.notFound(w, r)
			return
		}
		hr.serverError(w, r, err)
		return
	}

	// Start transaction to update problem with its tags
	tx, err := hr.db.Begin(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := hr.queries.WithTx(tx)

	// Update the code problem
	problem, err := qtx.UpdateCodeProblem(r.Context(), store.UpdateCodeProblemParams{
		ID:               toPgtypeUUID(problemID),
		Title:            req.Title,
		ProblemStatement: req.ProblemStatement,
		Difficulty:       req.Difficulty,
	})
	if err != nil {
		hr.logger.Error("failed to update code problem", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Delete existing problem tags and recreate
	err = qtx.DeleteCodeProblemTagsByProblemID(r.Context(), problem.ID)
	if err != nil {
		hr.logger.Error("failed to delete problem tags", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Create problem tags
	for _, tagIDStr := range req.TagIDs {
		tagID, err := uuid.Parse(tagIDStr)
		if err != nil {
			hr.badRequest(w, r, fmt.Errorf("invalid tag_id: %s", tagIDStr))
			return
		}

		err = qtx.CreateCodeProblemTag(r.Context(), store.CreateCodeProblemTagParams{
			CodeProblemID: problem.ID,
			TagID:         toPgtypeUUID(tagID),
		})
		if err != nil {
			hr.logger.Error("failed to create problem tag", "err", err, "tag_id", tagID)
			hr.serverError(w, r, err)
			return
		}
	}

	// Commit transaction
	if err = tx.Commit(r.Context()); err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("successfully updated code problem",
		"problem_id", problem.ID.Bytes,
		"title", problem.Title)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Success: true,
		Msg:     "Code problem updated successfully",
		Data: map[string]interface{}{
			"problem_id": uuid.UUID(problem.ID.Bytes).String(),
			"title":      problem.Title,
		},
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

// DeleteProblemHandler deletes an existing code problem
// This handler is for admin use only
func (hr *HandlerRepo) DeleteProblemHandler(w http.ResponseWriter, r *http.Request) {
	problemIDStr := chi.URLParam(r, "problem_id")
	problemID, err := uuid.Parse(problemIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid problem ID"))
		return
	}

	// Check if problem exists
	_, err = hr.queries.GetCodeProblemByID(r.Context(), toPgtypeUUID(problemID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hr.notFound(w, r)
			return
		}
		hr.serverError(w, r, err)
		return
	}

	err = hr.queries.DeleteCodeProblem(r.Context(), toPgtypeUUID(problemID))
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("successfully deleted code problem", "problem_id", problemID)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    nil,
		Success: true,
		Msg:     "Code problem deleted successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

// validateUpdateProblemRequest validates the update problem request
func validateUpdateProblemRequest(req *UpdateProblemRequest) error {
	if req.Title == "" {
		return errors.New("title is required")
	}
	if req.ProblemStatement == "" {
		return errors.New("problem_statement is required")
	}
	if req.Difficulty < 1 || req.Difficulty > 3 {
		return errors.New("difficulty must be between 1 and 3")
	}
	return nil
}

// validateCreateProblemRequest validates the create problem request
func validateCreateProblemRequest(req *CreateProblemRequest) error {
	if req.Title == "" {
		return errors.New("title is required")
	}
	if req.ProblemStatement == "" {
		return errors.New("problem_statement is required")
	}
	if req.Difficulty < 1 || req.Difficulty > 3 {
		return errors.New("difficulty must be between 1 and 3")
	}
	if len(req.LanguageDetails) == 0 {
		return errors.New("at least one language detail is required")
	}

	// Validate each language detail
	for i, langDetail := range req.LanguageDetails {
		if langDetail.LanguageID == "" {
			return fmt.Errorf("language_details[%d].language_id is required", i)
		}
		if langDetail.SolutionCode == "" {
			return fmt.Errorf("language_details[%d].solution_code is required", i)
		}
		if len(langDetail.TestCases) == 0 {
			return fmt.Errorf("language_details[%d] must have at least one test case", i)
		}
		for j, testCase := range langDetail.TestCases {
			if testCase.Input == "" {
				return fmt.Errorf("language_details[%d].test_cases[%d].input is required", i, j)
			}
			if testCase.ExpectedOutput == "" {
				return fmt.Errorf("language_details[%d].test_cases[%d].expected_output is required", i, j)
			}
		}
	}

	return nil
}

// validateAllTestSolutions validates test solutions for all languages
func (hr *HandlerRepo) validateAllTestSolutions(ctx context.Context, req *CreateProblemRequest) (map[string]*ExecutionValidationResult, error) {
	results := make(map[string]*ExecutionValidationResult)

	for _, langDetail := range req.LanguageDetails {
		result, err := hr.validateTestSolution(ctx, &langDetail)
		if err != nil {
			return nil, fmt.Errorf("failed to validate solution for language %s: %w", langDetail.LanguageID, err)
		}
		results[langDetail.LanguageID] = result
	}

	return results, nil
}

// validateTestSolution runs a single language's test solution against its test cases using the executor
func (hr *HandlerRepo) validateTestSolution(ctx context.Context, langDetail *ProblemLanguageDetailInput) (*ExecutionValidationResult, error) {
	// Get language information from database
	langID, err := uuid.Parse(langDetail.LanguageID)
	if err != nil {
		return nil, fmt.Errorf("invalid language_id: %w", err)
	}

	lang, err := hr.queries.GetLanguageByID(ctx, toPgtypeUUID(langID))
	if err != nil {
		return nil, fmt.Errorf("failed to get language: %w", err)
	}

	// Convert test cases to protobuf format
	pbTestCases := make([]*pb.TestCase, len(langDetail.TestCases))
	for i, tc := range langDetail.TestCases {
		pbTestCases[i] = &pb.TestCase{
			Input:          tc.Input,
			ExpectedOutput: tc.ExpectedOutput,
		}
	}

	// Execute the test solution
	execRequest := &pb.ExecuteRequest{
		Language:     lang.Name,
		Code:         langDetail.SolutionCode,
		DriverCode:   langDetail.DriverCode,
		CompileCmd:   lang.CompileCmd,
		RunCmd:       lang.RunCmd,
		TempFileDir:  lang.TempFileDir.String,
		TempFileName: lang.TempFileName.String,
		TestCases:    pbTestCases,
	}

	hr.logger.Info("validating test solution", "language", lang.Name, "test_cases", len(pbTestCases))

	execResult, err := hr.executorClient.ExecuteCode(ctx, execRequest)
	if err != nil {
		return nil, fmt.Errorf("executor failed: %w", err)
	}

	// Analyze execution result
	totalTests := len(pbTestCases)
	executionLog := fmt.Sprintf("stdout: %s\nstderr: %s\nmessage: %s\n",
		execResult.Stdout, execResult.Stderr, execResult.Message)

	validationResult := &ExecutionValidationResult{
		Success:      execResult.Success,
		TotalTests:   totalTests,
		ExecutionLog: executionLog,
	}

	if execResult.Success {
		validationResult.PassedTests = totalTests
		validationResult.Message = fmt.Sprintf("All %d test cases passed successfully", totalTests)
	} else {
		validationResult.PassedTests = 0
		validationResult.Message = fmt.Sprintf("Test solution failed: %s (error: %s)",
			execResult.Message, execResult.ErrorType)
	}

	return validationResult, nil
}

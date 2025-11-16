package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/response"
)

var (
	ErrInvalidRequest error = errors.New("Invalid request")
	ErrInternalServer error = errors.New("Internal server error")
)

func (hr *HandlerRepo) reportServerError(r *http.Request, err error) {
	var (
		message = err.Error()
		method  = r.Method
		url     = r.URL.String()
		trace   = string(debug.Stack())
	)

	requestAttrs := slog.Group("request", "method", method, "url", url)
	hr.logger.Error(message, requestAttrs, "trace", trace)
}

func (hr *HandlerRepo) errorMessage(w http.ResponseWriter, r *http.Request, status int, message string, headers http.Header) {
	message = strings.ToUpper(message[:1]) + message[1:]

	err := response.JSONWithHeaders(w, response.JSONResponseParameters{
		Success: false,
		Status:  status,
		Msg:     message,
		ErrMsg:  message,
	}, headers)
	if err != nil {
		hr.reportServerError(r, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (hr *HandlerRepo) invalidCredentials(w http.ResponseWriter, r *http.Request) {
	message := "invalid credentials"
	hr.errorMessage(w, r, http.StatusBadRequest, message, nil)
}

func (hr *HandlerRepo) serverError(w http.ResponseWriter, r *http.Request, err error) {
	// log error to std out
	hr.reportServerError(r, err)

	message := "The server encountered a problem and could not process your request"
	hr.errorMessage(w, r, http.StatusInternalServerError, message, nil)
}

func (hr *HandlerRepo) notFound(w http.ResponseWriter, r *http.Request) {
	message := "The requested resource could not be found"
	hr.errorMessage(w, r, http.StatusNotFound, message, nil)
}

func (hr *HandlerRepo) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	message := fmt.Sprintf("The %s method is not supported for this resource", r.Method)
	hr.errorMessage(w, r, http.StatusMethodNotAllowed, message, nil)
}

func (hr *HandlerRepo) badRequest(w http.ResponseWriter, r *http.Request, err error) {
	hr.errorMessage(w, r, http.StatusBadRequest, err.Error(), nil)
}

func (hr *HandlerRepo) basicAuthenticationRequired(w http.ResponseWriter, r *http.Request) {
	headers := make(http.Header)
	headers.Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)

	message := "You must be authenticated to access this resource"
	hr.errorMessage(w, r, http.StatusUnauthorized, message, headers)
}

func (hr *HandlerRepo) unauthorized(w http.ResponseWriter, r *http.Request) {
	message := "You must be authenticated to access this resource"
	hr.errorMessage(w, r, http.StatusUnauthorized, message, nil)
}

func (hr *HandlerRepo) forbidden(w http.ResponseWriter, r *http.Request) {
	message := "You do not have permission to access this resource"
	hr.errorMessage(w, r, http.StatusForbidden, message, nil)
}

package handlers

import (
	"net/http"
)

func (h *HandlerRepo) HealthcheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

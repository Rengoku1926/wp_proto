package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/errs"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/google/uuid"
)

type UserHandler struct {
	userService *service.UserService
}

func NewUserHandler(userService *service.UserService) *UserHandler {
    return &UserHandler{userService: userService}
}

func (h *UserHandler) HandleRegister(w http.ResponseWriter, r *http.Request){
	var req registerRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil{
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "Invalid JSON body"})
		return 
	}

	user, err := h.userService.RegisterUser(r.Context(), req.Username)
	if err != nil {
        switch {
        case errors.Is(err, errs.ErrValidation):
            writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
        case errors.Is(err, errs.ErrDuplicate):
            writeJSON(w, http.StatusConflict, errorResponse{Error: "username already taken"})
        default:
            writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
        }
        return
    }
	writeJSON(w, http.StatusCreated, user)
}

func (h *UserHandler) HandleGetUserById(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")

	id, err := uuid.Parse(idStr)
	if err != nil{
		 writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid user id"})
        return
	}

	user, err := h.userService.GetUser(r.Context(), id)
	if err != nil {
        if errors.Is(err, errs.ErrNotFound) {
            writeJSON(w, http.StatusNotFound, errorResponse{Error: "user not found"})
            return
        }
        writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
        return
    }

	writeJSON(w, http.StatusOK, user)
}



func writeJSON(w http.ResponseWriter, status int, data any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(data)
}

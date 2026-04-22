package handler

type registerRequest struct{
	Username string `json:"username"`
}

type errorResponse struct{
	Error string `json:"error"`
} 
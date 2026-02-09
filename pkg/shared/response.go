package shared

import (
	"encoding/json"
	"net/http"
	"time"
)

type Response struct {
	Service   string `json:"service"`
	Status    string `json:"status"`
	Port      string `json:"port"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message,omitempty"`
}

func JSON(w http.ResponseWriter, status int, resp Response) {
	resp.Timestamp = time.Now().UTC().Format(time.RFC3339)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

package turns

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kirin7002/app_personal_agent/contract"
)

func UtteranceHandler(service contract.TurnHandler, auth contract.Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		scheme, bearer, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(bearer) == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var input struct {
			SessionID string `json:"session_id"`
			VehicleID string `json:"vehicle_id"`
			RequestID string `json:"request_id"`
			Text      string `json:"text"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF ||
			input.SessionID == "" || input.RequestID == "" || len(input.RequestID) > 128 || strings.TrimSpace(input.Text) == "" {
			http.Error(w, "invalid utterance", http.StatusBadRequest)
			return
		}
		scope, err := auth.Authenticate(r.Context(), bearer, input.SessionID, input.VehicleID)
		if err != nil || !scope.Valid() || scope.SessionID != input.SessionID ||
			(input.VehicleID != "" && scope.VehicleID != input.VehicleID) {
			http.Error(w, "session or vehicle unavailable", http.StatusForbidden)
			return
		}
		reply, err := service.HandleVehicleTurn(r.Context(), contract.VehicleTurn{
			Scope: scope, VoiceTurnID: "text-" + stableID(scopeKey(scope), input.RequestID),
			RequestID:       input.RequestID,
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "text", InputItemID: input.RequestID, Text: input.Text},
		})
		if err != nil {
			var business *contract.BusinessError
			if errors.As(err, &business) {
				w.Header().Set("Content-Type", "application/json")
				status := http.StatusConflict
				if business.Code == contract.InvalidArgument {
					status = http.StatusBadRequest
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(business)
			} else {
				http.Error(w, "vehicle turn unavailable", http.StatusBadGateway)
			}
			return
		}
		body, err := json.Marshal(reply)
		if err != nil {
			http.Error(w, "cannot encode reply", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprintf(w, "id: %s\nevent: speech.reply\ndata: %s\n\n", stableID(scopeKey(scope), input.RequestID), body)
	})
}

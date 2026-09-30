package contract

import (
	"errors"
	"net/http"

	"wagering/internal/domain"
)

// ErrorBody is the stable transport shape for errors. The handler adds a
// correlation ID when one is available.
type ErrorBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationId,omitempty"`
}

func HTTPError(err error, correlationID string) (int, ErrorBody) {
	var de *domain.Error
	if errors.As(err, &de) {
		switch de.Kind {
		case domain.ErrKindConflict:
			return http.StatusConflict, ErrorBody{Code: string(de.Code), Message: de.Message, CorrelationID: correlationID}
		case domain.ErrKindRejected:
			return http.StatusUnprocessableEntity, ErrorBody{Code: string(de.Code), Message: de.Message, CorrelationID: correlationID}
		case domain.ErrKindTransient:
			return http.StatusServiceUnavailable, ErrorBody{Code: "TEMPORARILY_UNAVAILABLE", Message: "temporarily unavailable", CorrelationID: correlationID}
		case domain.ErrKindPermanent:
			return http.StatusInternalServerError, ErrorBody{Code: "PERMANENT_FAILURE", Message: "operation failed", CorrelationID: correlationID}
		}
		return http.StatusBadRequest, ErrorBody{Code: string(de.Code), Message: de.Message, CorrelationID: correlationID}
	}
	return http.StatusBadRequest, ErrorBody{Code: "INVALID_REQUEST", Message: err.Error(), CorrelationID: correlationID}
}

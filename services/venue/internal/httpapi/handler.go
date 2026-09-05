package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/domain"
	"mealroute/venue/internal/service"

	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
)

// Handler implements the generated API owned by the demo venue.
type Handler struct {
	service *service.Service
}

func NewHandler(application *service.Service) *Handler {
	return &Handler{service: application}
}

func NewRouter(application *service.Service) (http.Handler, error) {
	spec, err := venueapi.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("load embedded venue OpenAPI: %w", err)
	}
	generated := venueapi.HandlerWithOptions(NewHandler(application), venueapi.StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		},
	})
	validator := nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		DoNotValidateServers: true,
		Options: openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			MultiError:         true,
		},
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, _ *http.Request, options nethttpmiddleware.ErrorHandlerOpts) {
			status, code := validationProblem(options.StatusCode, err)
			writeProblem(w, status, code, err.Error())
		},
	})
	return validator(generated), nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, apiError *domain.Error) {
	writeProblem(w, statusFor(apiError.Code), apiError.Code, apiError.Message)
}

func writeProblem(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, venueapi.ProblemDetails{Code: code, Message: message})
}

func validationProblem(status int, err error) (int, string) {
	if strings.Contains(strings.ToLower(err.Error()), "method not allowed") {
		return http.StatusMethodNotAllowed, "method_not_allowed"
	}
	if status < http.StatusBadRequest {
		status = http.StatusBadRequest
	}
	switch status {
	case http.StatusNotFound:
		return status, "route_not_found"
	case http.StatusMethodNotAllowed:
		return status, "method_not_allowed"
	default:
		return status, "invalid_request"
	}
}

func statusFor(code string) int {
	switch code {
	case "order_not_found":
		return http.StatusNotFound
	case "invalid_order_transition":
		return http.StatusConflict
	case "platform_unavailable":
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

func writeBadRequest(w http.ResponseWriter, message string) {
	writeProblem(w, http.StatusBadRequest, "invalid_request", message)
}

func (h *Handler) GetVenueHealth(w http.ResponseWriter, r *http.Request) {
	version := "0.1.0"
	writeJSON(w, http.StatusOK, venueapi.HealthResponse{Status: venueapi.Ok, Service: "demo-venue", Version: &version})
}

func (h *Handler) GetLocalMenu(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.Menu(r.Context()))
}

func (h *Handler) ListLocalOrders(w http.ResponseWriter, r *http.Request, params venueapi.ListLocalOrdersParams) {
	status := ""
	if params.Status != nil {
		status = string(*params.Status)
	}
	limit := int32(0)
	if params.Limit != nil {
		limit = *params.Limit
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = string(*params.Cursor)
	}
	page, apiError := h.service.ListOrders(r.Context(), status, cursor, limit)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) GetLocalOrder(w http.ResponseWriter, r *http.Request, venueOrderId venueapi.VenueOrderId) {
	order, apiError := h.service.GetOrder(r.Context(), venueOrderId)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *Handler) UpdateLocalOrderStatus(w http.ResponseWriter, r *http.Request, venueOrderId venueapi.VenueOrderId) {
	var request venueapi.UpdateVenueOrderStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeBadRequest(w, "request body must be valid JSON")
		return
	}
	order, apiError := h.service.UpdateOrderStatus(r.Context(), venueOrderId, request.Status, request.Reason)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

var _ venueapi.ServerInterface = (*Handler)(nil)

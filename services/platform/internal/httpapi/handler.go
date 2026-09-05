package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/domain"
	"mealroute/platform/internal/service"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
)

// Handler implements the generated platform API on top of the repository.
// The generated file remains the source of routing and request binding.
type Handler struct {
	service *service.Service
}

func NewHandler(application *service.Service) *Handler {
	return &Handler{service: application}
}

func NewRouter(application *service.Service) (http.Handler, error) {
	spec, err := platformapi.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("load embedded platform OpenAPI: %w", err)
	}
	generated := platformapi.HandlerWithOptions(NewHandler(application), platformapi.StdHTTPServerOptions{
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
	writeJSON(w, status, platformapi.ProblemDetails{Code: code, Message: message})
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
	case "invalid_request":
		return http.StatusBadRequest
	case "invalid_venue_api_key":
		return http.StatusUnauthorized
	case "venue_not_found", "order_not_found":
		return http.StatusNotFound
	case "venue_closed", "item_not_found", "item_unavailable":
		return http.StatusUnprocessableEntity
	case "internal_error":
		return http.StatusInternalServerError
	default:
		return http.StatusConflict
	}
}

func writeBadRequest(w http.ResponseWriter, message string) {
	writeProblem(w, http.StatusBadRequest, "invalid_request", message)
}

func decodeJSONToResponse(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeBadRequest(w, "request body must be valid JSON")
		return false
	}
	return true
}

func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		writeBadRequest(w, "request body must be valid JSON")
		return false
	}
	return true
}

func (h *Handler) authenticatedVenue(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	venueID, apiError := h.service.AuthenticateVenue(r.Context(), r.Header.Get("X-Venue-API-Key"))
	if apiError != nil {
		writeError(w, apiError)
		return uuid.Nil, false
	}
	return venueID, true
}

func (h *Handler) GetPlatformHealth(w http.ResponseWriter, r *http.Request) {
	version := "0.1.0"
	writeJSON(w, http.StatusOK, platformapi.HealthResponse{Status: platformapi.Ok, Service: "platform", Version: &version})
}

func (h *Handler) ListVenues(w http.ResponseWriter, r *http.Request, params platformapi.ListVenuesParams) {
	city := ""
	if params.City != nil {
		city = *params.City
	}
	limit := int32(0)
	if params.Limit != nil {
		limit = *params.Limit
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = string(*params.Cursor)
	}
	page, apiError := h.service.ListVenues(r.Context(), city, cursor, limit)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) GetVenue(w http.ResponseWriter, r *http.Request, venueId platformapi.VenueId) {
	venue, apiError := h.service.GetVenue(r.Context(), venueId)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, venue)
}

func (h *Handler) GetVenueMenu(w http.ResponseWriter, r *http.Request, venueId platformapi.VenueId) {
	menu, apiError := h.service.GetMenu(r.Context(), venueId)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, menu)
}

func (h *Handler) ListCustomerOrders(w http.ResponseWriter, r *http.Request, params platformapi.ListCustomerOrdersParams) {
	limit := int32(0)
	if params.Limit != nil {
		limit = *params.Limit
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = string(*params.Cursor)
	}
	page, apiError := h.service.ListCustomerOrders(r.Context(), params.XUserId, cursor, limit)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request, params platformapi.CreateOrderParams) {
	var request platformapi.CreateOrderRequest
	if !decodeJSONToResponse(w, r, &request) {
		return
	}
	order, apiError := h.service.CreateOrder(r.Context(), params.XUserId, params.IdempotencyKey, request)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusCreated, order)
}

func (h *Handler) GetCustomerOrder(w http.ResponseWriter, r *http.Request, orderId platformapi.OrderId, params platformapi.GetCustomerOrderParams) {
	order, apiError := h.service.GetCustomerOrder(r.Context(), params.XUserId, orderId)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *Handler) CancelCustomerOrder(w http.ResponseWriter, r *http.Request, orderId platformapi.OrderId, params platformapi.CancelCustomerOrderParams) {
	var request platformapi.CancelOrderRequest
	if !decodeOptionalJSON(w, r, &request) {
		return
	}
	order, apiError := h.service.CancelCustomerOrder(r.Context(), params.XUserId, orderId, params.IdempotencyKey, request.Reason)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *Handler) SyncPartnerMenu(w http.ResponseWriter, r *http.Request) {
	venueID, ok := h.authenticatedVenue(w, r)
	if !ok {
		return
	}
	var request platformapi.MenuSyncRequest
	if !decodeJSONToResponse(w, r, &request) {
		return
	}
	response, apiError := h.service.SyncPartnerMenu(r.Context(), venueID, request)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) ListPartnerOrders(w http.ResponseWriter, r *http.Request, params platformapi.ListPartnerOrdersParams) {
	venueID, ok := h.authenticatedVenue(w, r)
	if !ok {
		return
	}
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
	page, apiError := h.service.ListPartnerOrders(r.Context(), venueID, status, cursor, limit)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) ListPartnerOrderEvents(w http.ResponseWriter, r *http.Request, params platformapi.ListPartnerOrderEventsParams) {
	venueID, ok := h.authenticatedVenue(w, r)
	if !ok {
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = string(*params.Cursor)
	}
	limit := int32(0)
	if params.Limit != nil {
		limit = *params.Limit
	}
	page, apiError := h.service.ListPartnerOrderEvents(r.Context(), venueID, cursor, limit)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) AcceptPartnerOrder(w http.ResponseWriter, r *http.Request, orderId platformapi.OrderId, params platformapi.AcceptPartnerOrderParams) {
	venueID, ok := h.authenticatedVenue(w, r)
	if !ok {
		return
	}
	var request platformapi.AcceptOrderRequest
	if !decodeJSONToResponse(w, r, &request) {
		return
	}
	order, apiError := h.service.AcceptPartnerOrder(r.Context(), venueID, orderId, params.IdempotencyKey, request)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *Handler) RejectPartnerOrder(w http.ResponseWriter, r *http.Request, orderId platformapi.OrderId, params platformapi.RejectPartnerOrderParams) {
	venueID, ok := h.authenticatedVenue(w, r)
	if !ok {
		return
	}
	var request platformapi.RejectOrderRequest
	if !decodeJSONToResponse(w, r, &request) {
		return
	}
	order, apiError := h.service.RejectPartnerOrder(r.Context(), venueID, orderId, params.IdempotencyKey, request)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *Handler) UpdatePartnerOrderStatus(w http.ResponseWriter, r *http.Request, orderId platformapi.OrderId, params platformapi.UpdatePartnerOrderStatusParams) {
	venueID, ok := h.authenticatedVenue(w, r)
	if !ok {
		return
	}
	var request platformapi.UpdateOrderStatusRequest
	if !decodeJSONToResponse(w, r, &request) {
		return
	}
	order, apiError := h.service.UpdatePartnerOrderStatus(r.Context(), venueID, orderId, params.IdempotencyKey, request)
	if apiError != nil {
		writeError(w, apiError)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

var _ platformapi.ServerInterface = (*Handler)(nil)

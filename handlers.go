package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

type TransferRequest struct {
	SKU          string `json:"sku"`
	FromLocation string `json:"fromLocation"`
	ToLocation   string `json:"toLocation"`
	Quantity     int    `json:"quantity"`
}

type TransferResponse struct {
	Status       string `json:"status"`
	SKU          string `json:"sku"`
	FromLocation string `json:"fromLocation"`
	ToLocation   string `json:"toLocation"`
	Quantity     int    `json:"quantity"`
}

type InventoryResponse struct {
	SKU       string       `json:"sku"`
	Locations []StockLevel `json:"locations"`
	Total     int          `json:"total"`
}

type HealthResponse struct {
	Status string    `json:"status"`
	Pool   PoolStats `json:"pool"`
}

type Handler struct {
	store *Store
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "UP", Pool: h.store.Stats()})
}

func (h *Handler) inventory(w http.ResponseWriter, r *http.Request) {
	sku := r.PathValue("sku")
	ctx, cancel := context.WithTimeout(r.Context(), acquireTimeout)
	defer cancel()

	levels, err := h.store.StockLevels(ctx, h.store.pool, sku)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if len(levels) == 0 {
		writeError(w, http.StatusNotFound, "unknown SKU "+sku)
		return
	}

	total := 0
	for _, l := range levels {
		total += l.Quantity
	}
	writeJSON(w, http.StatusOK, InventoryResponse{SKU: sku, Locations: levels, Total: total})
}

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) {
	var req TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := r.Context()
	conn, err := h.store.Acquire(ctx)
	if err != nil {
		internalError(w, r, err)
		return
	}
	defer conn.Release() // added: grants a guarantee of returning connection to the pool

	levels, err := h.store.StockLevels(ctx, conn, req.SKU)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if len(levels) == 0 {
		writeError(w, http.StatusNotFound, "unknown SKU "+req.SKU)
		return
	}
	stock := make(map[string]int, len(levels))
	for _, l := range levels {
		stock[l.Location] = l.Quantity
	}
	for _, location := range []string{req.FromLocation, req.ToLocation} {
		if _, ok := stock[location]; !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s is not stocked at %s", req.SKU, location))
			return
		}
	}

	if problem := checkTransferAllowed(ctx, req); problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}

	if available := stock[req.FromLocation]; available < req.Quantity {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("insufficient stock at %s: available %d, requested %d", req.FromLocation, available, req.Quantity))
		return
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		internalError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	// providing tx instead of conn
	if err := h.store.MoveStock(ctx, tx, req.SKU, req.FromLocation, req.ToLocation, req.Quantity); err != nil {
		internalError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, r, err)
		return
	}
	// moved conn.Release() to the start of the handler

	writeJSON(w, http.StatusOK, TransferResponse{
		Status:       "COMPLETED",
		SKU:          req.SKU,
		FromLocation: req.FromLocation,
		ToLocation:   req.ToLocation,
		Quantity:     req.Quantity,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

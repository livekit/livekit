package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/ipfs/go-log/v2"
)

type RelevantNodesHandler struct {
	nodeProvider *NodeProvider
	logger       *log.ZapEventLogger
}

func NewRelevantNodesHandler(nodeProvider *NodeProvider, logger *log.ZapEventLogger) *RelevantNodesHandler {
	return &RelevantNodesHandler{
		nodeProvider: nodeProvider,
		logger:       logger,
	}
}

type SearchRelevantNodeRequest struct {
	IP string `json:"ip"`
}

type SearchRelevantNodeResponse struct {
	Domain string `json:"domain"`
}

func (h *RelevantNodesHandler) HTTPHandler(w http.ResponseWriter, r *http.Request) {
	var req SearchRelevantNodeRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		handleError(w, http.StatusBadRequest, fmt.Errorf("unmarshal body %w", err))
		return
	}

	if net.ParseIP(req.IP) == nil {
		handleError(w, http.StatusBadRequest, errors.New("parse ip error"))
		return
	}

	node, err := h.nodeProvider.FetchRelevant(r.Context(), req.IP)
	if err != nil {
		handleError(w, http.StatusBadRequest, fmt.Errorf("fetch relevant nodes %w", err))
		return
	}

	response, err := json.Marshal(SearchRelevantNodeResponse{Domain: node.Domain})
	if err != nil {
		handleError(w, http.StatusBadRequest, fmt.Errorf("marshal response %w", err))
		return
	}

	_, err = w.Write(response)
	if err != nil {
		handleError(w, http.StatusBadRequest, fmt.Errorf("send response %w", err))
		return
	}
}

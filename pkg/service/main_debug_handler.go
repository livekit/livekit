package service

import (
	"fmt"
	"net/http"
	"context"
	"time"
	"github.com/olekukonko/tablewriter"
	"github.com/ipfs/go-log/v2"
)

type MainDebugHandler struct {
	nodeProvider *NodeProvider
	logger       *log.ZapEventLogger
}

func NewMainDebugHandler(nodeProvider *NodeProvider, logger *log.ZapEventLogger) *MainDebugHandler {
	return &MainDebugHandler{
		nodeProvider: nodeProvider,
		logger:       logger,
	}
}

func (h *MainDebugHandler) nodeHTTPHandler(w http.ResponseWriter, r *http.Request) {

	nodes, err := h.nodeProvider.List(context.Background())
	if err != nil {
		handleError(w, http.StatusBadRequest, fmt.Errorf("send response %w", err))
		return
	}

	table := tablewriter.NewWriter(w)
	table.SetRowLine(true)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{
		"ID",
		"Participants",
		"Domain",
		"IP",
		"Country",
		"Latitude",
		"Longitude",
		"CreatedAt",
	})

	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})

	for _, node := range nodes {
		table.Append([]string{
			node.Id,
			fmt.Sprintf("%d", node.Participants),
			node.Domain,
			node.IP,
			node.Country,
			fmt.Sprintf("%f", node.Latitude),
			fmt.Sprintf("%f", node.Longitude),
			node.CreatedAt.Format(time.RFC3339),
		})
	}

	table.Render()
	return
}

func (h *MainDebugHandler) peerHTTPHandler(w http.ResponseWriter, r *http.Request) {

	nodes, err := h.nodeProvider.List(context.Background())
	if err != nil {
		handleError(w, http.StatusBadRequest, fmt.Errorf("send response %w", err))
		return
	}


	table := tablewriter.NewWriter(w)
	table.SetRowLine(true)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{
		"ID",
		"Participants",
		"Domain",
		"IP",
		"Country",
		"Latitude",
		"Longitude",
		"CreatedAt",
	})

	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})

	for _, node := range nodes {
		table.Append([]string{
			node.Id,
			fmt.Sprintf("%d", node.Participants),
			node.Domain,
			node.IP,
			node.Country,
			fmt.Sprintf("%f", node.Latitude),
			fmt.Sprintf("%f", node.Longitude),
			node.CreatedAt.Format(time.RFC3339),
		})
	}

	table.Render()
	return
}

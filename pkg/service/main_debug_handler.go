package service

import (
	"fmt"
	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/ipfs/go-log/v2"
	"github.com/olekukonko/tablewriter"
	"net/http"
	"time"
)

type MainDebugHandler struct {
	nodeProvider   *NodeProvider
	clientProvider *ClientProvider
	logger         *log.ZapEventLogger
	db             *p2p_database.DB
}

func NewMainDebugHandler(db *p2p_database.DB, nodeProvider *NodeProvider, clientProvider *ClientProvider, logger *log.ZapEventLogger) *MainDebugHandler {
	return &MainDebugHandler{
		nodeProvider:   nodeProvider,
		clientProvider: clientProvider,
		logger:         logger,
		db:             db,
	}
}

func (h *MainDebugHandler) clientHTTPHandler(w http.ResponseWriter, r *http.Request) {
	clients, err := h.clientProvider.List(r.Context())
	if err != nil {
		handleError(w, http.StatusBadRequest, fmt.Errorf("send response %w", err))
		return
	}

	table := tablewriter.NewWriter(w)
	table.SetRowLine(true)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{
		"Key",
		"Until",
		"Active",
		"Key",
	})

	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})

	for _, client := range clients {
		table.Append([]string{
			client.Key,
			fmt.Sprintf("%d", client.Until),
			fmt.Sprintf("%v", client.Active),
			fmt.Sprintf("%s", client.Limit),
		})
	}

	table.Render()
	return
}

func (h *MainDebugHandler) nodeHTTPHandler(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.nodeProvider.List(r.Context())
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

	table := tablewriter.NewWriter(w)
	table.SetRowLine(true)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{
		"ID",
		"Remote address",
	})
	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})

	for _, node := range h.db.ConnectedPeers() {
		table.Append([]string{
			node.ID.String(),
			node.Addrs[0].String(),
		})
	}

	table.Render()
	return
}

package service

import (
	"fmt"
	p2p_database "github.com/dTelecom/p2p-database"
	"github.com/olekukonko/tablewriter"
	"net/http"
)

type MainDebugHandler struct {
	nodeProvider   *NodeProvider
	clientProvider *ClientProvider
	db             *p2p_database.DB
}

func NewMainDebugHandler(nodeProvider *NodeProvider, clientProvider *ClientProvider, db *p2p_database.DB) *MainDebugHandler {
	return &MainDebugHandler{
		nodeProvider:   nodeProvider,
		clientProvider: clientProvider,
		db:             db,
	}
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
		"City",
		"Latitude",
		"Longitude",
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
			node.City,
			fmt.Sprintf("%f", node.Latitude),
			fmt.Sprintf("%f", node.Longitude),
		})
	}

	table.Render()
	return
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
		"Address",
		"Until",
		"Limit",
	})

	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})

	for address, client := range clients {
		table.Append([]string{
			address,
			fmt.Sprintf("%d", client.Until),
			fmt.Sprintf("%d", client.Limit),
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

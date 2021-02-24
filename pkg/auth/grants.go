package auth

type VideoGrant struct {
	// actions on rooms
	RoomCreate bool `json:"roomCreate,omitempty"`
	RoomList   bool `json:"roomList,omitempty"`

	// actions on a particular room
	RoomAdmin bool   `json:"roomAdmin,omitempty"`
	RoomJoin  bool   `json:"roomJoin,omitempty"`
	Room      string `json:"room,omitempty"`
}

type ClaimGrants struct {
	Identity string                 `json:"-"`
	Video    *VideoGrant            `json:"video,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

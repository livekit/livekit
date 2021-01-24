package auth

type VideoGrant struct {
	RoomCreate bool   `json:"roomCreate,omitempty"`
	RoomJoin   bool   `json:"roomJoin,omitempty"`
	RoomList   bool   `json:"roomList,omitempty"`
	Room       string `json:"room,omitempty"`
}

type ClaimGrants struct {
	Identity string      `json:"-"`
	Video    *VideoGrant `json:"video,omitempty"`
}

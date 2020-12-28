package auth

type VideoGrant struct {
	RoomCreate bool   `json:"room_create,omitempty"`
	RoomJoin   bool   `json:"room_join,omitempty"`
	Room       string `json:"room,omitempty"`
}

type ClaimGrants struct {
	Video *VideoGrant `json:"video,omitempty"`
}

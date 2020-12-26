package auth

type GrantClaims struct {
	RoomCreate bool   `json:"room_create,omitempty"`
	RoomJoin   bool   `json:"room_join,omitempty"`
	Room       string `json:"room"`
}

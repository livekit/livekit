package p2p

type RoomCommunicator interface {
	ForEachPeer(peerHandler func(peerId string))
	SendMessage(peerId string, message interface{}) (string, error)
	OnMessage(messageHandler func(message interface{}, fromPeerId string, eventId string))
}

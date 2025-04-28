package p2p

type RoomCommunicator interface {
	ForEachPeer(peerHandler func(peerId string))
	OnMessage(messageHandler func(message interface{}, fromPeerId string, eventId string))
	SendMessage(peerId string, message interface{}) (string, error)
}

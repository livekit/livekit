package p2p

type RoomCommunicator interface {
	ForEachPeer(peerHandler func(peerId string))
	OnMessage(messageHandler func(message []byte, fromPeerId string, eventId string))
	SendMessage(peerId string, message []byte) (string, error)
}

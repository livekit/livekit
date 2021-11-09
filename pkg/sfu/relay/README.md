# Relay

`ion-sfu` supports relaying tracks to other ion-SFUs or other services using the ORTC API.

Using this api allows to quickly send the stream to other services by signaling a single request, after that all the following negotiations are handled internally.

## API

### Relay Peer

The relay peer shares common methods with the Webrtc PeerConnection, so it should be straight forward to use. To create a new relay peer follow below example
:
```go
 // Meta holds all the related information of the peer you want to relay.
 meta := PeerMeta{
	      PeerID : "super-villain-1",
	      SessionID : "world-domination",
        } 
 // config will hold pion/webrtc related structs required for the connection. 
 // you should fill according your requirements or leave the defaults.
 config := &PeerConfig{} 
 peer, err := NewPeer(meta, config)
 handleErr(err)
 
 // Now before working with the peer you need to signal the peer to 
 // your remote sever, the signaling can be whatever method you want (gRPC, RESt, pubsub, etc..)
 signalFunc= func (meta PeerMeta, signal []byte) ([]byte, error){
   if meta.session== "world-domination"{
      	return RelayToLegionOfDoom(meta, signal)
   }	
   return nil, errors.New("not supported")
 }
 
 // The remote peer should create a new Relay Peer with the metadata and call Answer.
 if err:= peer.Offer(signalFunc); err!=nil{
 	handleErr(err)
 }
 
 // If there are no errors, relay peer offer some convenience methods to communicate with
 // Relayed peer.
 
 // Emit will fire and forget to the request event
 peer.Emit("evil-plan-1", data)
 // Request will wait for a remote answer, use a time cancelled
 // context to not block forever if peer does not answer
 ans,err:= peer.Request(ctx, "evil-plan-2", data)
 // To listen to remote event just attach the callback to peer
 peer.OnRequest( func (event string, msg Message){
 	// to access to request data
 	msg.Paylod()
 	// to reply the request
 	msg.Reply(...)
 })
 
 // The Relay Peer also has some convenience callbacks to manage the peer lifespan.
 
 // Peer OnClose is called when the remote peer connection is closed, or the Close method is called
 peer.OnClose(func())
 // Peer OnReady is called when the relay peer is ready to start negotiating tracks, data channels and request
 // is highly recommended to attach all the initialization logic to this callback
 peer.OnReady(func())
 
 // To add or receive tracks or data channels the API is similar to webrtc Peer Connection, just listen
 // to the required callbacks
 peer.OnDataChannel(f func(channel *webrtc.DataChannel))
 peer.OnTrack(f func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver))
 // Make sure to call below methods after the OnReady callback fired.
 peer.CreateDataChannel(label string)
 peer.AddTrack(receiver *webrtc.RTPReceiver, remoteTrack *webrtc.TrackRemote,
localTrack webrtc.TrackLocal) (*webrtc.RTPSender, error)
```

### ION-SFU integration

ION-SFU offers some convenience methods for relaying peers in a very simple way.

To relay a peer just call `Peer.Publisher().Relay(...)` then signal the data to the remote SFU and ingest the data using:

`session.AddRelayPeer(peerID string, signalData []byte) ([]byte, error)` 

set the []byte response from the method as the response of the signaling. And is ready, everytime a peer joins to the new SFU will negotiate the relayed stream.

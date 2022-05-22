package service

import (
    "context"
    "encoding/json"
    "net/http"
    "time"

    "github.com/livekit/protocol/auth"
    "github.com/livekit/protocol/livekit"
)

type RequestInternalToken struct {
    Method       string `json:"kind,omitempty"`
    CallKey      string `json:"call_key,omitempty"`
    NameCalled   string `json:"name_called,omitempty"`
    NameIdentity string `json:"name_identity,omitempty"`

    InternalKey    string `json:"key"`
    InternalSecret string `json:"secret"`
}

type ResultInternalToken struct {
    Location string `json:"location,omitempty"`
    Token    string `json:"token,omitempty"`
}

func (s *LivekitServer) internalToken(w http.ResponseWriter, r *http.Request) {
    decoder := json.NewDecoder(r.Body)
    var req RequestInternalToken
    if err := decoder.Decode(&req); nil != err {
        w.WriteHeader(http.StatusNotAcceptable)
        return
    }
    grant := auth.VideoGrant{}
    grant.Room = req.CallKey
    grant.RoomJoin = true
    at := auth.NewAccessToken(req.InternalKey, req.InternalSecret)
    switch req.Method {
    case "start":
        grant.RoomCreate = true
    case "invite":
    }
    at.AddGrant(&grant).SetIdentity(req.NameIdentity).SetName(req.NameCalled).SetMetadata("metadata" + req.NameIdentity).SetValidFor(time.Hour)
    t, err := at.ToJWT()
    if err != nil {
        w.WriteHeader(http.StatusNotAcceptable)
        return
    }
    result := ResultInternalToken{}
    result.Location = "wss"
    result.Token = t
    if bytes, err := json.Marshal(result); nil != err {
        w.WriteHeader(http.StatusInternalServerError)
        return
    } else {
        w.WriteHeader(http.StatusOK)
        w.Header().Add("Content-Type", "application/json")
        _, _ = w.Write(bytes)
    }
}

type RequestInternalTracks struct {
    CallKey      livekit.RoomName            `json:"call_key,omitempty"`
    NameIdentity livekit.ParticipantIdentity `json:"name_identity,omitempty"`
}

type ResultInternalTrack struct {
    Kind      string `json:"kind,omitempty"`
    TrackName string `json:"track_name,omitempty"`
}

type ResultInternalTracks struct {
    Tracks []ResultInternalTrack `json:"tracks"`
}

func (s *LivekitServer) internalTracks(w http.ResponseWriter, r *http.Request) {
    decoder := json.NewDecoder(r.Body)
    var req RequestInternalTracks
    if err := decoder.Decode(&req); nil != err {
        w.WriteHeader(http.StatusNotAcceptable)
        return
    }
    participant, err := s.roomManager.roomStore.LoadParticipant(context.Background(), req.CallKey, req.NameIdentity)
    if err != nil {
        w.WriteHeader(http.StatusInternalServerError)
        return
    }
    result := ResultInternalTracks{}
    for _, x := range participant.Tracks {
        result.Tracks = append(result.Tracks, ResultInternalTrack{TrackName: x.Sid, Kind: x.Type.String()})
    }
    if bytes, err := json.Marshal(result); nil != err {
        w.WriteHeader(http.StatusInternalServerError)
        return
    } else {
        w.WriteHeader(http.StatusOK)
        w.Header().Add("Content-Type", "application/json")
        _, _ = w.Write(bytes)
    }
}

type RequestInternalPlayers struct {
    CallKey livekit.RoomName `json:"call_key,omitempty"`
}

type ResultInternalPlayer struct {
    NameIdentity string `json:"name_identity,omitempty"`
    NameCalled   string `json:"name_called,omitempty"`
}

type ResultInternalPlayers struct {
    Players []ResultInternalPlayer `json:"players,omitempty"`
}

func (s *LivekitServer) internalPlayers(w http.ResponseWriter, r *http.Request) {
    decoder := json.NewDecoder(r.Body)
    var req RequestInternalPlayers
    if err := decoder.Decode(&req); nil != err {
        w.WriteHeader(http.StatusNotAcceptable)
        return
    }
    participants, err := s.roomManager.roomStore.ListParticipants(context.Background(), req.CallKey)
    if err != nil {
        return
    }
    result := ResultInternalPlayers{}
    for _, x := range participants {
        result.Players = append(result.Players, ResultInternalPlayer{NameIdentity: x.Identity, NameCalled: x.Name})
    }
    if bytes, err := json.Marshal(result); nil != err {
        w.WriteHeader(http.StatusInternalServerError)
        return
    } else {
        w.WriteHeader(http.StatusOK)
        w.Header().Add("Content-Type", "application/json")
        _, _ = w.Write(bytes)
    }
}

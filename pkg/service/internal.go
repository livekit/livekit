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
    Method    string `json:"kind,omitempty"`
    KCall     string `json:"k_call,omitempty"`
    KIdentity string `json:"k_identity,omitempty"`
    KnownAs   string `json:"known_as,omitempty"`

    InternalKey    string `json:"key,omitempty"`
    InternalSecret string `json:"secret,omitempty"`
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
    grant.Room = req.KCall
    grant.RoomJoin = true
    at := auth.NewAccessToken(req.InternalKey, req.InternalSecret)
    switch req.Method {
    case "start":
        grant.RoomCreate = true
    case "invite":
    }
    at.AddGrant(&grant).SetIdentity(req.KIdentity).SetName(req.KnownAs).SetMetadata("metadata" + req.KIdentity).SetValidFor(time.Hour)
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
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write(bytes)
    }
}

type RequestInternalTracks struct {
    KCall     livekit.RoomName            `json:"k_call,omitempty"`
    KIdentity livekit.ParticipantIdentity `json:"k_identity,omitempty"`
}

type ResultInternalTrack struct {
    Kind     string `json:"kind,omitempty"`
    TrackKey string `json:"track_key,omitempty"`
}

type ResultInternalTracks struct {
    Tracks []ResultInternalTrack `json:"tracks,omitempty"`
}

func (s *LivekitServer) internalTracks(w http.ResponseWriter, r *http.Request) {
    decoder := json.NewDecoder(r.Body)
    var req RequestInternalTracks
    if err := decoder.Decode(&req); nil != err {
        w.WriteHeader(http.StatusNotAcceptable)
        return
    }
    participant, err := s.roomManager.roomStore.LoadParticipant(context.Background(), req.KCall, req.KIdentity)
    if err != nil {
        w.WriteHeader(http.StatusInternalServerError)
        return
    }
    result := ResultInternalTracks{}
    for _, x := range participant.Tracks {
        result.Tracks = append(result.Tracks, ResultInternalTrack{TrackKey: x.Sid, Kind: x.Type.String()})
    }
    if bytes, err := json.Marshal(result); nil != err {
        w.WriteHeader(http.StatusInternalServerError)
        return
    } else {
        w.WriteHeader(http.StatusOK)
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write(bytes)
    }
}

type RequestInternalPlayers struct {
    KCall livekit.RoomName `json:"k_call,omitempty"`
}

type ResultInternalPlayer struct {
    KIdentity string `json:"k_identity,omitempty"`
    KnownAs   string `json:"known_as,omitempty"`
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
    participants, err := s.roomManager.roomStore.ListParticipants(context.Background(), req.KCall)
    if err != nil {
        return
    }
    result := ResultInternalPlayers{}
    for _, x := range participants {
        result.Players = append(result.Players, ResultInternalPlayer{KIdentity: x.Identity, KnownAs: x.Name})
    }
    if bytes, err := json.Marshal(result); nil != err {
        w.WriteHeader(http.StatusInternalServerError)
        return
    } else {
        w.WriteHeader(http.StatusOK)
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write(bytes)
    }
}

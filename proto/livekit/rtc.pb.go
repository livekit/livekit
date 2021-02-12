// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.25.0
// 	protoc        v3.14.0
// source: rtc.proto

package livekit

import (
	proto "github.com/golang/protobuf/proto"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

// This is a compile-time assertion that a sufficiently up-to-date version
// of the legacy proto package is being used.
const _ = proto.ProtoPackageIsVersion4

type SignalTarget int32

const (
	SignalTarget_PUBLISHER  SignalTarget = 0
	SignalTarget_SUBSCRIBER SignalTarget = 1
)

// Enum value maps for SignalTarget.
var (
	SignalTarget_name = map[int32]string{
		0: "PUBLISHER",
		1: "SUBSCRIBER",
	}
	SignalTarget_value = map[string]int32{
		"PUBLISHER":  0,
		"SUBSCRIBER": 1,
	}
)

func (x SignalTarget) Enum() *SignalTarget {
	p := new(SignalTarget)
	*p = x
	return p
}

func (x SignalTarget) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (SignalTarget) Descriptor() protoreflect.EnumDescriptor {
	return file_rtc_proto_enumTypes[0].Descriptor()
}

func (SignalTarget) Type() protoreflect.EnumType {
	return &file_rtc_proto_enumTypes[0]
}

func (x SignalTarget) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use SignalTarget.Descriptor instead.
func (SignalTarget) EnumDescriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{0}
}

type SignalRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// Types that are assignable to Message:
	//	*SignalRequest_Offer
	//	*SignalRequest_Answer
	//	*SignalRequest_Trickle
	//	*SignalRequest_AddTrack
	//	*SignalRequest_Mute
	//	*SignalRequest_MuteSubscribed
	Message isSignalRequest_Message `protobuf_oneof:"message"`
}

func (x *SignalRequest) Reset() {
	*x = SignalRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *SignalRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*SignalRequest) ProtoMessage() {}

func (x *SignalRequest) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use SignalRequest.ProtoReflect.Descriptor instead.
func (*SignalRequest) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{0}
}

func (m *SignalRequest) GetMessage() isSignalRequest_Message {
	if m != nil {
		return m.Message
	}
	return nil
}

func (x *SignalRequest) GetOffer() *SessionDescription {
	if x, ok := x.GetMessage().(*SignalRequest_Offer); ok {
		return x.Offer
	}
	return nil
}

func (x *SignalRequest) GetAnswer() *SessionDescription {
	if x, ok := x.GetMessage().(*SignalRequest_Answer); ok {
		return x.Answer
	}
	return nil
}

func (x *SignalRequest) GetTrickle() *TrickleRequest {
	if x, ok := x.GetMessage().(*SignalRequest_Trickle); ok {
		return x.Trickle
	}
	return nil
}

func (x *SignalRequest) GetAddTrack() *AddTrackRequest {
	if x, ok := x.GetMessage().(*SignalRequest_AddTrack); ok {
		return x.AddTrack
	}
	return nil
}

func (x *SignalRequest) GetMute() *MuteTrackRequest {
	if x, ok := x.GetMessage().(*SignalRequest_Mute); ok {
		return x.Mute
	}
	return nil
}

func (x *SignalRequest) GetMuteSubscribed() *MuteTrackRequest {
	if x, ok := x.GetMessage().(*SignalRequest_MuteSubscribed); ok {
		return x.MuteSubscribed
	}
	return nil
}

type isSignalRequest_Message interface {
	isSignalRequest_Message()
}

type SignalRequest_Offer struct {
	// initial join exchange, for publisher
	Offer *SessionDescription `protobuf:"bytes,1,opt,name=offer,proto3,oneof"`
}

type SignalRequest_Answer struct {
	// participant answering publisher offer
	Answer *SessionDescription `protobuf:"bytes,2,opt,name=answer,proto3,oneof"`
}

type SignalRequest_Trickle struct {
	Trickle *TrickleRequest `protobuf:"bytes,3,opt,name=trickle,proto3,oneof"`
}

type SignalRequest_AddTrack struct {
	AddTrack *AddTrackRequest `protobuf:"bytes,4,opt,name=add_track,json=addTrack,proto3,oneof"`
}

type SignalRequest_Mute struct {
	// mute the participant's own tracks
	Mute *MuteTrackRequest `protobuf:"bytes,5,opt,name=mute,proto3,oneof"`
}

type SignalRequest_MuteSubscribed struct {
	// mute a track client is subscribed to
	MuteSubscribed *MuteTrackRequest `protobuf:"bytes,6,opt,name=mute_subscribed,json=muteSubscribed,proto3,oneof"`
}

func (*SignalRequest_Offer) isSignalRequest_Message() {}

func (*SignalRequest_Answer) isSignalRequest_Message() {}

func (*SignalRequest_Trickle) isSignalRequest_Message() {}

func (*SignalRequest_AddTrack) isSignalRequest_Message() {}

func (*SignalRequest_Mute) isSignalRequest_Message() {}

func (*SignalRequest_MuteSubscribed) isSignalRequest_Message() {}

type SignalResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// Types that are assignable to Message:
	//	*SignalResponse_Join
	//	*SignalResponse_Answer
	//	*SignalResponse_Offer
	//	*SignalResponse_Trickle
	//	*SignalResponse_Update
	//	*SignalResponse_TrackPublished
	Message isSignalResponse_Message `protobuf_oneof:"message"`
}

func (x *SignalResponse) Reset() {
	*x = SignalResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *SignalResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*SignalResponse) ProtoMessage() {}

func (x *SignalResponse) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use SignalResponse.ProtoReflect.Descriptor instead.
func (*SignalResponse) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{1}
}

func (m *SignalResponse) GetMessage() isSignalResponse_Message {
	if m != nil {
		return m.Message
	}
	return nil
}

func (x *SignalResponse) GetJoin() *JoinResponse {
	if x, ok := x.GetMessage().(*SignalResponse_Join); ok {
		return x.Join
	}
	return nil
}

func (x *SignalResponse) GetAnswer() *SessionDescription {
	if x, ok := x.GetMessage().(*SignalResponse_Answer); ok {
		return x.Answer
	}
	return nil
}

func (x *SignalResponse) GetOffer() *SessionDescription {
	if x, ok := x.GetMessage().(*SignalResponse_Offer); ok {
		return x.Offer
	}
	return nil
}

func (x *SignalResponse) GetTrickle() *TrickleRequest {
	if x, ok := x.GetMessage().(*SignalResponse_Trickle); ok {
		return x.Trickle
	}
	return nil
}

func (x *SignalResponse) GetUpdate() *ParticipantUpdate {
	if x, ok := x.GetMessage().(*SignalResponse_Update); ok {
		return x.Update
	}
	return nil
}

func (x *SignalResponse) GetTrackPublished() *TrackPublishedResponse {
	if x, ok := x.GetMessage().(*SignalResponse_TrackPublished); ok {
		return x.TrackPublished
	}
	return nil
}

type isSignalResponse_Message interface {
	isSignalResponse_Message()
}

type SignalResponse_Join struct {
	// sent when join is accepted
	Join *JoinResponse `protobuf:"bytes,1,opt,name=join,proto3,oneof"`
}

type SignalResponse_Answer struct {
	// sent when server answers publisher
	Answer *SessionDescription `protobuf:"bytes,2,opt,name=answer,proto3,oneof"`
}

type SignalResponse_Offer struct {
	// sent when server is sending subscriber an offer
	Offer *SessionDescription `protobuf:"bytes,3,opt,name=offer,proto3,oneof"`
}

type SignalResponse_Trickle struct {
	// sent when an ICE candidate is available
	Trickle *TrickleRequest `protobuf:"bytes,4,opt,name=trickle,proto3,oneof"`
}

type SignalResponse_Update struct {
	// sent when participants in the room has changed
	Update *ParticipantUpdate `protobuf:"bytes,5,opt,name=update,proto3,oneof"`
}

type SignalResponse_TrackPublished struct {
	// sent to the participant when their track has been published
	TrackPublished *TrackPublishedResponse `protobuf:"bytes,6,opt,name=track_published,json=trackPublished,proto3,oneof"`
}

func (*SignalResponse_Join) isSignalResponse_Message() {}

func (*SignalResponse_Answer) isSignalResponse_Message() {}

func (*SignalResponse_Offer) isSignalResponse_Message() {}

func (*SignalResponse_Trickle) isSignalResponse_Message() {}

func (*SignalResponse_Update) isSignalResponse_Message() {}

func (*SignalResponse_TrackPublished) isSignalResponse_Message() {}

type AddTrackRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// client ID of track, to match it when RTC track is received
	Cid  string    `protobuf:"bytes,1,opt,name=cid,proto3" json:"cid,omitempty"`
	Name string    `protobuf:"bytes,2,opt,name=name,proto3" json:"name,omitempty"`
	Type TrackType `protobuf:"varint,3,opt,name=type,proto3,enum=livekit.TrackType" json:"type,omitempty"`
}

func (x *AddTrackRequest) Reset() {
	*x = AddTrackRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[2]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AddTrackRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AddTrackRequest) ProtoMessage() {}

func (x *AddTrackRequest) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[2]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AddTrackRequest.ProtoReflect.Descriptor instead.
func (*AddTrackRequest) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{2}
}

func (x *AddTrackRequest) GetCid() string {
	if x != nil {
		return x.Cid
	}
	return ""
}

func (x *AddTrackRequest) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *AddTrackRequest) GetType() TrackType {
	if x != nil {
		return x.Type
	}
	return TrackType_AUDIO
}

type TrickleRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	CandidateInit string       `protobuf:"bytes,1,opt,name=candidateInit,proto3" json:"candidateInit,omitempty"`
	Target        SignalTarget `protobuf:"varint,2,opt,name=target,proto3,enum=livekit.SignalTarget" json:"target,omitempty"`
}

func (x *TrickleRequest) Reset() {
	*x = TrickleRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[3]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *TrickleRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*TrickleRequest) ProtoMessage() {}

func (x *TrickleRequest) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[3]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use TrickleRequest.ProtoReflect.Descriptor instead.
func (*TrickleRequest) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{3}
}

func (x *TrickleRequest) GetCandidateInit() string {
	if x != nil {
		return x.CandidateInit
	}
	return ""
}

func (x *TrickleRequest) GetTarget() SignalTarget {
	if x != nil {
		return x.Target
	}
	return SignalTarget_PUBLISHER
}

type MuteTrackRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Sid   string `protobuf:"bytes,1,opt,name=sid,proto3" json:"sid,omitempty"`
	Muted bool   `protobuf:"varint,2,opt,name=muted,proto3" json:"muted,omitempty"`
}

func (x *MuteTrackRequest) Reset() {
	*x = MuteTrackRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[4]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *MuteTrackRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*MuteTrackRequest) ProtoMessage() {}

func (x *MuteTrackRequest) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[4]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use MuteTrackRequest.ProtoReflect.Descriptor instead.
func (*MuteTrackRequest) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{4}
}

func (x *MuteTrackRequest) GetSid() string {
	if x != nil {
		return x.Sid
	}
	return ""
}

func (x *MuteTrackRequest) GetMuted() bool {
	if x != nil {
		return x.Muted
	}
	return false
}

type NegotiationRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields
}

func (x *NegotiationRequest) Reset() {
	*x = NegotiationRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[5]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *NegotiationRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*NegotiationRequest) ProtoMessage() {}

func (x *NegotiationRequest) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[5]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use NegotiationRequest.ProtoReflect.Descriptor instead.
func (*NegotiationRequest) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{5}
}

type JoinResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Room              *Room              `protobuf:"bytes,1,opt,name=room,proto3" json:"room,omitempty"`
	Participant       *ParticipantInfo   `protobuf:"bytes,2,opt,name=participant,proto3" json:"participant,omitempty"`
	OtherParticipants []*ParticipantInfo `protobuf:"bytes,3,rep,name=other_participants,json=otherParticipants,proto3" json:"other_participants,omitempty"`
	ServerVersion     string             `protobuf:"bytes,4,opt,name=server_version,json=serverVersion,proto3" json:"server_version,omitempty"`
}

func (x *JoinResponse) Reset() {
	*x = JoinResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[6]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *JoinResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*JoinResponse) ProtoMessage() {}

func (x *JoinResponse) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[6]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use JoinResponse.ProtoReflect.Descriptor instead.
func (*JoinResponse) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{6}
}

func (x *JoinResponse) GetRoom() *Room {
	if x != nil {
		return x.Room
	}
	return nil
}

func (x *JoinResponse) GetParticipant() *ParticipantInfo {
	if x != nil {
		return x.Participant
	}
	return nil
}

func (x *JoinResponse) GetOtherParticipants() []*ParticipantInfo {
	if x != nil {
		return x.OtherParticipants
	}
	return nil
}

func (x *JoinResponse) GetServerVersion() string {
	if x != nil {
		return x.ServerVersion
	}
	return ""
}

type TrackPublishedResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Cid   string     `protobuf:"bytes,1,opt,name=cid,proto3" json:"cid,omitempty"`
	Track *TrackInfo `protobuf:"bytes,2,opt,name=track,proto3" json:"track,omitempty"`
}

func (x *TrackPublishedResponse) Reset() {
	*x = TrackPublishedResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[7]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *TrackPublishedResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*TrackPublishedResponse) ProtoMessage() {}

func (x *TrackPublishedResponse) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[7]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use TrackPublishedResponse.ProtoReflect.Descriptor instead.
func (*TrackPublishedResponse) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{7}
}

func (x *TrackPublishedResponse) GetCid() string {
	if x != nil {
		return x.Cid
	}
	return ""
}

func (x *TrackPublishedResponse) GetTrack() *TrackInfo {
	if x != nil {
		return x.Track
	}
	return nil
}

type SessionDescription struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Type string `protobuf:"bytes,1,opt,name=type,proto3" json:"type,omitempty"` // "answer" | "offer" | "pranswer" | "rollback"
	Sdp  string `protobuf:"bytes,2,opt,name=sdp,proto3" json:"sdp,omitempty"`
}

func (x *SessionDescription) Reset() {
	*x = SessionDescription{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[8]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *SessionDescription) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*SessionDescription) ProtoMessage() {}

func (x *SessionDescription) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[8]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use SessionDescription.ProtoReflect.Descriptor instead.
func (*SessionDescription) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{8}
}

func (x *SessionDescription) GetType() string {
	if x != nil {
		return x.Type
	}
	return ""
}

func (x *SessionDescription) GetSdp() string {
	if x != nil {
		return x.Sdp
	}
	return ""
}

type ParticipantUpdate struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Participants []*ParticipantInfo `protobuf:"bytes,1,rep,name=participants,proto3" json:"participants,omitempty"`
}

func (x *ParticipantUpdate) Reset() {
	*x = ParticipantUpdate{}
	if protoimpl.UnsafeEnabled {
		mi := &file_rtc_proto_msgTypes[9]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *ParticipantUpdate) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ParticipantUpdate) ProtoMessage() {}

func (x *ParticipantUpdate) ProtoReflect() protoreflect.Message {
	mi := &file_rtc_proto_msgTypes[9]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ParticipantUpdate.ProtoReflect.Descriptor instead.
func (*ParticipantUpdate) Descriptor() ([]byte, []int) {
	return file_rtc_proto_rawDescGZIP(), []int{9}
}

func (x *ParticipantUpdate) GetParticipants() []*ParticipantInfo {
	if x != nil {
		return x.Participants
	}
	return nil
}

var File_rtc_proto protoreflect.FileDescriptor

var file_rtc_proto_rawDesc = []byte{
	0x0a, 0x09, 0x72, 0x74, 0x63, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x07, 0x6c, 0x69, 0x76,
	0x65, 0x6b, 0x69, 0x74, 0x1a, 0x0b, 0x6d, 0x6f, 0x64, 0x65, 0x6c, 0x2e, 0x70, 0x72, 0x6f, 0x74,
	0x6f, 0x22, 0xeb, 0x02, 0x0a, 0x0d, 0x53, 0x69, 0x67, 0x6e, 0x61, 0x6c, 0x52, 0x65, 0x71, 0x75,
	0x65, 0x73, 0x74, 0x12, 0x33, 0x0a, 0x05, 0x6f, 0x66, 0x66, 0x65, 0x72, 0x18, 0x01, 0x20, 0x01,
	0x28, 0x0b, 0x32, 0x1b, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x53, 0x65, 0x73,
	0x73, 0x69, 0x6f, 0x6e, 0x44, 0x65, 0x73, 0x63, 0x72, 0x69, 0x70, 0x74, 0x69, 0x6f, 0x6e, 0x48,
	0x00, 0x52, 0x05, 0x6f, 0x66, 0x66, 0x65, 0x72, 0x12, 0x35, 0x0a, 0x06, 0x61, 0x6e, 0x73, 0x77,
	0x65, 0x72, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x1b, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b,
	0x69, 0x74, 0x2e, 0x53, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e, 0x44, 0x65, 0x73, 0x63, 0x72, 0x69,
	0x70, 0x74, 0x69, 0x6f, 0x6e, 0x48, 0x00, 0x52, 0x06, 0x61, 0x6e, 0x73, 0x77, 0x65, 0x72, 0x12,
	0x33, 0x0a, 0x07, 0x74, 0x72, 0x69, 0x63, 0x6b, 0x6c, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x17, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x54, 0x72, 0x69, 0x63, 0x6b,
	0x6c, 0x65, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x48, 0x00, 0x52, 0x07, 0x74, 0x72, 0x69,
	0x63, 0x6b, 0x6c, 0x65, 0x12, 0x37, 0x0a, 0x09, 0x61, 0x64, 0x64, 0x5f, 0x74, 0x72, 0x61, 0x63,
	0x6b, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x18, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69,
	0x74, 0x2e, 0x41, 0x64, 0x64, 0x54, 0x72, 0x61, 0x63, 0x6b, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73,
	0x74, 0x48, 0x00, 0x52, 0x08, 0x61, 0x64, 0x64, 0x54, 0x72, 0x61, 0x63, 0x6b, 0x12, 0x2f, 0x0a,
	0x04, 0x6d, 0x75, 0x74, 0x65, 0x18, 0x05, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x19, 0x2e, 0x6c, 0x69,
	0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x4d, 0x75, 0x74, 0x65, 0x54, 0x72, 0x61, 0x63, 0x6b, 0x52,
	0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x48, 0x00, 0x52, 0x04, 0x6d, 0x75, 0x74, 0x65, 0x12, 0x44,
	0x0a, 0x0f, 0x6d, 0x75, 0x74, 0x65, 0x5f, 0x73, 0x75, 0x62, 0x73, 0x63, 0x72, 0x69, 0x62, 0x65,
	0x64, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x19, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69,
	0x74, 0x2e, 0x4d, 0x75, 0x74, 0x65, 0x54, 0x72, 0x61, 0x63, 0x6b, 0x52, 0x65, 0x71, 0x75, 0x65,
	0x73, 0x74, 0x48, 0x00, 0x52, 0x0e, 0x6d, 0x75, 0x74, 0x65, 0x53, 0x75, 0x62, 0x73, 0x63, 0x72,
	0x69, 0x62, 0x65, 0x64, 0x42, 0x09, 0x0a, 0x07, 0x6d, 0x65, 0x73, 0x73, 0x61, 0x67, 0x65, 0x22,
	0xeb, 0x02, 0x0a, 0x0e, 0x53, 0x69, 0x67, 0x6e, 0x61, 0x6c, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e,
	0x73, 0x65, 0x12, 0x2b, 0x0a, 0x04, 0x6a, 0x6f, 0x69, 0x6e, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x15, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x4a, 0x6f, 0x69, 0x6e, 0x52,
	0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x48, 0x00, 0x52, 0x04, 0x6a, 0x6f, 0x69, 0x6e, 0x12,
	0x35, 0x0a, 0x06, 0x61, 0x6e, 0x73, 0x77, 0x65, 0x72, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32,
	0x1b, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x53, 0x65, 0x73, 0x73, 0x69, 0x6f,
	0x6e, 0x44, 0x65, 0x73, 0x63, 0x72, 0x69, 0x70, 0x74, 0x69, 0x6f, 0x6e, 0x48, 0x00, 0x52, 0x06,
	0x61, 0x6e, 0x73, 0x77, 0x65, 0x72, 0x12, 0x33, 0x0a, 0x05, 0x6f, 0x66, 0x66, 0x65, 0x72, 0x18,
	0x03, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x1b, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e,
	0x53, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e, 0x44, 0x65, 0x73, 0x63, 0x72, 0x69, 0x70, 0x74, 0x69,
	0x6f, 0x6e, 0x48, 0x00, 0x52, 0x05, 0x6f, 0x66, 0x66, 0x65, 0x72, 0x12, 0x33, 0x0a, 0x07, 0x74,
	0x72, 0x69, 0x63, 0x6b, 0x6c, 0x65, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x17, 0x2e, 0x6c,
	0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x54, 0x72, 0x69, 0x63, 0x6b, 0x6c, 0x65, 0x52, 0x65,
	0x71, 0x75, 0x65, 0x73, 0x74, 0x48, 0x00, 0x52, 0x07, 0x74, 0x72, 0x69, 0x63, 0x6b, 0x6c, 0x65,
	0x12, 0x34, 0x0a, 0x06, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x18, 0x05, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x1a, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x50, 0x61, 0x72, 0x74, 0x69,
	0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x55, 0x70, 0x64, 0x61, 0x74, 0x65, 0x48, 0x00, 0x52, 0x06,
	0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x12, 0x4a, 0x0a, 0x0f, 0x74, 0x72, 0x61, 0x63, 0x6b, 0x5f,
	0x70, 0x75, 0x62, 0x6c, 0x69, 0x73, 0x68, 0x65, 0x64, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0b, 0x32,
	0x1f, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x54, 0x72, 0x61, 0x63, 0x6b, 0x50,
	0x75, 0x62, 0x6c, 0x69, 0x73, 0x68, 0x65, 0x64, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65,
	0x48, 0x00, 0x52, 0x0e, 0x74, 0x72, 0x61, 0x63, 0x6b, 0x50, 0x75, 0x62, 0x6c, 0x69, 0x73, 0x68,
	0x65, 0x64, 0x42, 0x09, 0x0a, 0x07, 0x6d, 0x65, 0x73, 0x73, 0x61, 0x67, 0x65, 0x22, 0x5f, 0x0a,
	0x0f, 0x41, 0x64, 0x64, 0x54, 0x72, 0x61, 0x63, 0x6b, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74,
	0x12, 0x10, 0x0a, 0x03, 0x63, 0x69, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x63,
	0x69, 0x64, 0x12, 0x12, 0x0a, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09,
	0x52, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x12, 0x26, 0x0a, 0x04, 0x74, 0x79, 0x70, 0x65, 0x18, 0x03,
	0x20, 0x01, 0x28, 0x0e, 0x32, 0x12, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x54,
	0x72, 0x61, 0x63, 0x6b, 0x54, 0x79, 0x70, 0x65, 0x52, 0x04, 0x74, 0x79, 0x70, 0x65, 0x22, 0x65,
	0x0a, 0x0e, 0x54, 0x72, 0x69, 0x63, 0x6b, 0x6c, 0x65, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74,
	0x12, 0x24, 0x0a, 0x0d, 0x63, 0x61, 0x6e, 0x64, 0x69, 0x64, 0x61, 0x74, 0x65, 0x49, 0x6e, 0x69,
	0x74, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0d, 0x63, 0x61, 0x6e, 0x64, 0x69, 0x64, 0x61,
	0x74, 0x65, 0x49, 0x6e, 0x69, 0x74, 0x12, 0x2d, 0x0a, 0x06, 0x74, 0x61, 0x72, 0x67, 0x65, 0x74,
	0x18, 0x02, 0x20, 0x01, 0x28, 0x0e, 0x32, 0x15, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74,
	0x2e, 0x53, 0x69, 0x67, 0x6e, 0x61, 0x6c, 0x54, 0x61, 0x72, 0x67, 0x65, 0x74, 0x52, 0x06, 0x74,
	0x61, 0x72, 0x67, 0x65, 0x74, 0x22, 0x3a, 0x0a, 0x10, 0x4d, 0x75, 0x74, 0x65, 0x54, 0x72, 0x61,
	0x63, 0x6b, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x12, 0x10, 0x0a, 0x03, 0x73, 0x69, 0x64,
	0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x73, 0x69, 0x64, 0x12, 0x14, 0x0a, 0x05, 0x6d,
	0x75, 0x74, 0x65, 0x64, 0x18, 0x02, 0x20, 0x01, 0x28, 0x08, 0x52, 0x05, 0x6d, 0x75, 0x74, 0x65,
	0x64, 0x22, 0x14, 0x0a, 0x12, 0x4e, 0x65, 0x67, 0x6f, 0x74, 0x69, 0x61, 0x74, 0x69, 0x6f, 0x6e,
	0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x22, 0xdd, 0x01, 0x0a, 0x0c, 0x4a, 0x6f, 0x69, 0x6e,
	0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x12, 0x21, 0x0a, 0x04, 0x72, 0x6f, 0x6f, 0x6d,
	0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x0d, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74,
	0x2e, 0x52, 0x6f, 0x6f, 0x6d, 0x52, 0x04, 0x72, 0x6f, 0x6f, 0x6d, 0x12, 0x3a, 0x0a, 0x0b, 0x70,
	0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x18, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x50, 0x61, 0x72, 0x74, 0x69,
	0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x49, 0x6e, 0x66, 0x6f, 0x52, 0x0b, 0x70, 0x61, 0x72, 0x74,
	0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x12, 0x47, 0x0a, 0x12, 0x6f, 0x74, 0x68, 0x65, 0x72,
	0x5f, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x73, 0x18, 0x03, 0x20,
	0x03, 0x28, 0x0b, 0x32, 0x18, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x50, 0x61,
	0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x49, 0x6e, 0x66, 0x6f, 0x52, 0x11, 0x6f,
	0x74, 0x68, 0x65, 0x72, 0x50, 0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x73,
	0x12, 0x25, 0x0a, 0x0e, 0x73, 0x65, 0x72, 0x76, 0x65, 0x72, 0x5f, 0x76, 0x65, 0x72, 0x73, 0x69,
	0x6f, 0x6e, 0x18, 0x04, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0d, 0x73, 0x65, 0x72, 0x76, 0x65, 0x72,
	0x56, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x22, 0x54, 0x0a, 0x16, 0x54, 0x72, 0x61, 0x63, 0x6b,
	0x50, 0x75, 0x62, 0x6c, 0x69, 0x73, 0x68, 0x65, 0x64, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73,
	0x65, 0x12, 0x10, 0x0a, 0x03, 0x63, 0x69, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03,
	0x63, 0x69, 0x64, 0x12, 0x28, 0x0a, 0x05, 0x74, 0x72, 0x61, 0x63, 0x6b, 0x18, 0x02, 0x20, 0x01,
	0x28, 0x0b, 0x32, 0x12, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x54, 0x72, 0x61,
	0x63, 0x6b, 0x49, 0x6e, 0x66, 0x6f, 0x52, 0x05, 0x74, 0x72, 0x61, 0x63, 0x6b, 0x22, 0x3a, 0x0a,
	0x12, 0x53, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e, 0x44, 0x65, 0x73, 0x63, 0x72, 0x69, 0x70, 0x74,
	0x69, 0x6f, 0x6e, 0x12, 0x12, 0x0a, 0x04, 0x74, 0x79, 0x70, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28,
	0x09, 0x52, 0x04, 0x74, 0x79, 0x70, 0x65, 0x12, 0x10, 0x0a, 0x03, 0x73, 0x64, 0x70, 0x18, 0x02,
	0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x73, 0x64, 0x70, 0x22, 0x51, 0x0a, 0x11, 0x50, 0x61, 0x72,
	0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x55, 0x70, 0x64, 0x61, 0x74, 0x65, 0x12, 0x3c,
	0x0a, 0x0c, 0x70, 0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x73, 0x18, 0x01,
	0x20, 0x03, 0x28, 0x0b, 0x32, 0x18, 0x2e, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2e, 0x50,
	0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x49, 0x6e, 0x66, 0x6f, 0x52, 0x0c,
	0x70, 0x61, 0x72, 0x74, 0x69, 0x63, 0x69, 0x70, 0x61, 0x6e, 0x74, 0x73, 0x2a, 0x2d, 0x0a, 0x0c,
	0x53, 0x69, 0x67, 0x6e, 0x61, 0x6c, 0x54, 0x61, 0x72, 0x67, 0x65, 0x74, 0x12, 0x0d, 0x0a, 0x09,
	0x50, 0x55, 0x42, 0x4c, 0x49, 0x53, 0x48, 0x45, 0x52, 0x10, 0x00, 0x12, 0x0e, 0x0a, 0x0a, 0x53,
	0x55, 0x42, 0x53, 0x43, 0x52, 0x49, 0x42, 0x45, 0x52, 0x10, 0x01, 0x42, 0x31, 0x5a, 0x2f, 0x67,
	0x69, 0x74, 0x68, 0x75, 0x62, 0x2e, 0x63, 0x6f, 0x6d, 0x2f, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69,
	0x74, 0x2f, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x2d, 0x73, 0x65, 0x72, 0x76, 0x65, 0x72,
	0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2f, 0x6c, 0x69, 0x76, 0x65, 0x6b, 0x69, 0x74, 0x62, 0x06,
	0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_rtc_proto_rawDescOnce sync.Once
	file_rtc_proto_rawDescData = file_rtc_proto_rawDesc
)

func file_rtc_proto_rawDescGZIP() []byte {
	file_rtc_proto_rawDescOnce.Do(func() {
		file_rtc_proto_rawDescData = protoimpl.X.CompressGZIP(file_rtc_proto_rawDescData)
	})
	return file_rtc_proto_rawDescData
}

var file_rtc_proto_enumTypes = make([]protoimpl.EnumInfo, 1)
var file_rtc_proto_msgTypes = make([]protoimpl.MessageInfo, 10)
var file_rtc_proto_goTypes = []interface{}{
	(SignalTarget)(0),              // 0: livekit.SignalTarget
	(*SignalRequest)(nil),          // 1: livekit.SignalRequest
	(*SignalResponse)(nil),         // 2: livekit.SignalResponse
	(*AddTrackRequest)(nil),        // 3: livekit.AddTrackRequest
	(*TrickleRequest)(nil),         // 4: livekit.TrickleRequest
	(*MuteTrackRequest)(nil),       // 5: livekit.MuteTrackRequest
	(*NegotiationRequest)(nil),     // 6: livekit.NegotiationRequest
	(*JoinResponse)(nil),           // 7: livekit.JoinResponse
	(*TrackPublishedResponse)(nil), // 8: livekit.TrackPublishedResponse
	(*SessionDescription)(nil),     // 9: livekit.SessionDescription
	(*ParticipantUpdate)(nil),      // 10: livekit.ParticipantUpdate
	(TrackType)(0),                 // 11: livekit.TrackType
	(*Room)(nil),                   // 12: livekit.Room
	(*ParticipantInfo)(nil),        // 13: livekit.ParticipantInfo
	(*TrackInfo)(nil),              // 14: livekit.TrackInfo
}
var file_rtc_proto_depIdxs = []int32{
	9,  // 0: livekit.SignalRequest.offer:type_name -> livekit.SessionDescription
	9,  // 1: livekit.SignalRequest.answer:type_name -> livekit.SessionDescription
	4,  // 2: livekit.SignalRequest.trickle:type_name -> livekit.TrickleRequest
	3,  // 3: livekit.SignalRequest.add_track:type_name -> livekit.AddTrackRequest
	5,  // 4: livekit.SignalRequest.mute:type_name -> livekit.MuteTrackRequest
	5,  // 5: livekit.SignalRequest.mute_subscribed:type_name -> livekit.MuteTrackRequest
	7,  // 6: livekit.SignalResponse.join:type_name -> livekit.JoinResponse
	9,  // 7: livekit.SignalResponse.answer:type_name -> livekit.SessionDescription
	9,  // 8: livekit.SignalResponse.offer:type_name -> livekit.SessionDescription
	4,  // 9: livekit.SignalResponse.trickle:type_name -> livekit.TrickleRequest
	10, // 10: livekit.SignalResponse.update:type_name -> livekit.ParticipantUpdate
	8,  // 11: livekit.SignalResponse.track_published:type_name -> livekit.TrackPublishedResponse
	11, // 12: livekit.AddTrackRequest.type:type_name -> livekit.TrackType
	0,  // 13: livekit.TrickleRequest.target:type_name -> livekit.SignalTarget
	12, // 14: livekit.JoinResponse.room:type_name -> livekit.Room
	13, // 15: livekit.JoinResponse.participant:type_name -> livekit.ParticipantInfo
	13, // 16: livekit.JoinResponse.other_participants:type_name -> livekit.ParticipantInfo
	14, // 17: livekit.TrackPublishedResponse.track:type_name -> livekit.TrackInfo
	13, // 18: livekit.ParticipantUpdate.participants:type_name -> livekit.ParticipantInfo
	19, // [19:19] is the sub-list for method output_type
	19, // [19:19] is the sub-list for method input_type
	19, // [19:19] is the sub-list for extension type_name
	19, // [19:19] is the sub-list for extension extendee
	0,  // [0:19] is the sub-list for field type_name
}

func init() { file_rtc_proto_init() }
func file_rtc_proto_init() {
	if File_rtc_proto != nil {
		return
	}
	file_model_proto_init()
	if !protoimpl.UnsafeEnabled {
		file_rtc_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*SignalRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*SignalResponse); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[2].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AddTrackRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[3].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*TrickleRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[4].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*MuteTrackRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[5].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*NegotiationRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[6].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*JoinResponse); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[7].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*TrackPublishedResponse); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[8].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*SessionDescription); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_rtc_proto_msgTypes[9].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*ParticipantUpdate); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
	}
	file_rtc_proto_msgTypes[0].OneofWrappers = []interface{}{
		(*SignalRequest_Offer)(nil),
		(*SignalRequest_Answer)(nil),
		(*SignalRequest_Trickle)(nil),
		(*SignalRequest_AddTrack)(nil),
		(*SignalRequest_Mute)(nil),
		(*SignalRequest_MuteSubscribed)(nil),
	}
	file_rtc_proto_msgTypes[1].OneofWrappers = []interface{}{
		(*SignalResponse_Join)(nil),
		(*SignalResponse_Answer)(nil),
		(*SignalResponse_Offer)(nil),
		(*SignalResponse_Trickle)(nil),
		(*SignalResponse_Update)(nil),
		(*SignalResponse_TrackPublished)(nil),
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_rtc_proto_rawDesc,
			NumEnums:      1,
			NumMessages:   10,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_rtc_proto_goTypes,
		DependencyIndexes: file_rtc_proto_depIdxs,
		EnumInfos:         file_rtc_proto_enumTypes,
		MessageInfos:      file_rtc_proto_msgTypes,
	}.Build()
	File_rtc_proto = out.File
	file_rtc_proto_rawDesc = nil
	file_rtc_proto_goTypes = nil
	file_rtc_proto_depIdxs = nil
}

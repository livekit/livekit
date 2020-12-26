package rtc_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

func TestRoomManager_CreateRoom(t *testing.T) {
	man := newRoomManager(t)

	t.Run("creation and duplicates", func(t *testing.T) {
		r := &livekit.CreateRoomRequest{Name: "basic"}
		rm, err := man.CreateRoom(r)
		assert.NoError(t, err)
		assert.NotNil(t, rm)

		rm, err = man.CreateRoom(r)
		assert.Equal(t, rtc.ErrInvalidRoomName, err)
		assert.Nil(t, rm)
	})

	t.Run("name is required", func(t *testing.T) {
		_, err := man.CreateRoom(&livekit.CreateRoomRequest{})
		assert.Equal(t, rtc.ErrInvalidRoomName, err)
	})
}

func TestRoomManager_GetRoomByName(t *testing.T) {
	man := newRoomManager(t)
	_, err := man.CreateRoom(&livekit.CreateRoomRequest{
		Name: "hello",
	})
	assert.NoError(t, err)

	rm := man.GetRoom("hello")
	assert.Equal(t, "hello", rm.Name)
}

func TestRoomManager_GetRoomWithConstraint(t *testing.T) {
	man := newRoomManager(t)
	rm, _ := man.CreateRoom(&livekit.CreateRoomRequest{
		Name: "hello",
	})

	t.Run("no constraint, get by id", func(t *testing.T) {
		r, err := man.GetRoomWithConstraint(rm.Sid, "")
		assert.NoError(t, err)
		assert.Equal(t, r, rm)
	})

	t.Run("no constraint, get by name", func(t *testing.T) {
		r, err := man.GetRoomWithConstraint(rm.Name, "")
		assert.NoError(t, err)
		assert.Equal(t, r, rm)
	})

	t.Run("no constraint, room doesn't exist", func(t *testing.T) {
		_, err := man.GetRoomWithConstraint("wtf", "")
		assert.Equal(t, rtc.ErrRoomNotFound, err)
	})

	t.Run("constraint, no name", func(t *testing.T) {
		r, err := man.GetRoomWithConstraint("", rm.Name)
		assert.NoError(t, err)
		assert.Equal(t, r, rm)
	})

	t.Run("constraint does not match name", func(t *testing.T) {
		_, err := man.GetRoomWithConstraint(rm.Name, "anotherroom")
		assert.Equal(t, rtc.ErrPermissionDenied, err)
	})
}

func newRoomManager(t *testing.T) *rtc.RoomManager {
	man, err := rtc.NewRoomManager(config.RTCConfig{}, "1.2.3.4")
	assert.NoError(t, err)
	return man
}

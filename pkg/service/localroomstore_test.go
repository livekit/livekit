package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/service"
)

func TestLocalRoomStore_GetParticipantId(t *testing.T) {
	s := service.NewLocalRoomStore()
	id, _ := s.GetParticipantId("room1", "p1")
	assert.NotEmpty(t, id)

	t.Run("diff room, same name returns a new ID", func(t *testing.T) {
		id2, _ := s.GetParticipantId("room2", "p1")
		assert.NotEmpty(t, id2)
		assert.NotEqual(t, id, id2)
	})

	t.Run("same room returns identical id", func(t *testing.T) {
		id2, _ := s.GetParticipantId("room1", "p1")
		assert.Equal(t, id, id2)
	})

	t.Run("same room with different name", func(t *testing.T) {
		id2, _ := s.GetParticipantId("room1", "p2")
		assert.NotEqual(t, id, id2)
	})
}

package rtc

import (
	"strings"
)

const (
	trackIdSeparator = "|"
)

func UnpackTrackId(packed string) (peerId string, trackId string) {
	parts := strings.Split(packed, trackIdSeparator)
	if len(parts) > 1 {
		return parts[0], packed[len(parts[0])+1:]
	}
	return "", packed
}

func PackTrackId(participantId, trackId string) string {
	return participantId + trackIdSeparator + trackId
}

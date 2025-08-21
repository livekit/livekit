package buffer

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractH26xVideoSize(t *testing.T) {
	type testcase struct {
		payload string
		width   uint32
		height  uint32
		isH264  bool
	}

	testcases := []testcase{
		{"eAAOZ0LAH4xoBQBboB4RCNQABGjOPIA=", 1280, 720, true},
		{"eAAPZ0LAFoxoCgL3lgHhEI1AAARozjyA", 640, 360, true},
		{"eAAOZ0LADIxoFBl54B4RCNQABGjOPIA=", 320, 180, true},
		{"YAEAGkABDAP//wFgAAADALAAAAMAAAMAXQAAGwJAAC9CAQMBYAAAAwCwAAADAAADAF0AAKACgIAtFiBu5FIy5+E9C+ob1SmoCAgIH8IBBAAHRAHAcvBbJA==", 1280, 720, false},
		{"YAEAGkABDAP//wFgAAADALAAAAMAAAMAPwAAGwJAADBCAQMBYAAAAwCwAAADAAADAD8AAKAFAgFx8uIG7kUjLn4T0L6hvVKagICAgfwgEEAAB0QBwHLwWyQ=", 640, 360, false},
		{"QgEDAWAAAAMAsAAAAwAAAwA8AACgCggMHz4gM7kUhi5+E9C+ob1Q/qoI9VQT6qoK9VVBfqqqDPVVVKagICAgfwgEEA==", 320, 180, false},
	}

	for _, tc := range testcases {
		payload, err := base64.StdEncoding.DecodeString(tc.payload)
		require.NoError(t, err)

		var sz VideoSize
		if tc.isH264 {
			sz = ExtractH264VideoSize(payload)
		} else {
			sz = ExtractH265VideoSize(payload)
		}
		require.Equal(t, tc.width, sz.Width)
		require.Equal(t, tc.height, sz.Height)
	}
}

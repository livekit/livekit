package livekit

import (
	_ "embed"
)

//go:embed GeoLite2-City.mmdb
var MixmindDatabase []byte

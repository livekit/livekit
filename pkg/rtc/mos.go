package rtc

import (
	"math"
)

// MOS score calculation is based on opentok-mos-estimator
// available @ https://github.com/wobbals/opentok-mos-estimator

func h(x float64) float64 {
	if x < 0.0 {
		return 0.0
	}
	return 1.0
}

func audioMos(R float64) float64 {
	if R < 0 {
		return 1
	}
	if R > 100 {
		return 4.5
	}

	return 1 + 0.035*R + 7.10/1000000*R*(R-60)*(100-R)

}

func calculateAudioScore(cur *MediaStats, prev *MediaStats) float64 {
	a := 0
	b := 19.8
	c := 29.7

	if cur == nil || prev == nil {
		return 0.0
	}

	totalAudioPackets := (cur.PacketsLost - prev.PacketsLost) + (cur.PacketsReceived - prev.PacketsReceived)

	if totalAudioPackets <= 0 {
		return 0.0
	}
	lostRatio := (cur.PacketsLost - prev.PacketsLost) / totalAudioPackets
	// take average delay for now
	delay := float64(cur.Delay / cur.NumReports)
	Id := 0.024*delay + 0.11*(delay-177.3)*h(delay-177.3)
	Ie := float64(a) + b*math.Log(float64(1)+c*float64(lostRatio))

	R := 94.2 - Id - Ie

	return audioMos(R)
}

func targetBitrateForPixelCount(pixelCount uint32) float64 {
	y := 2.069924867 * math.Pow(math.Log10(float64(pixelCount)), 0.6250223771)
	return math.Pow(10, y)
}

func calculateVideoScore(cur *MediaStats, prev *MediaStats, height uint32, weight uint32) float64 {
	if cur == nil || prev == nil {
		return 0.0
	}
	interval := lostUpdateDelta.Seconds()
	bytesIn := cur.BytesIn - prev.BytesIn
	bitRate := float64(bytesIn*8) / (interval / 1000)
	if bitRate < 3000 {
		return 0
	}
	pixelCount := height * weight
	targetBitrate := targetBitrateForPixelCount(pixelCount)
	if targetBitrate < 3000 {
		return 0
	}
	bitRate = math.Min(targetBitrate, bitRate)

	return (math.Log(bitRate/3000)/math.Log(targetBitrate/30000))*4 + 1
}

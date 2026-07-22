package pricing

// base32 is the geohash alphabet (no a, i, l, o).
const base32 = "0123456789bcdefghjkmnpqrstuvwxyz"

// Geohash encodes a lat/lng point to a geohash string of the given precision
// (number of base32 characters), using the standard bit-interleaving algorithm.
// No external dependency. Longitude and latitude bits are interleaved starting
// with longitude, 5 bits per output character.
func Geohash(lat, lng float64, precision int) string {
	latRange := [2]float64{-90, 90}
	lngRange := [2]float64{-180, 180}

	out := make([]byte, 0, precision)
	var bit, ch int
	evenBit := true // true ⇒ next bit refines longitude

	for len(out) < precision {
		if evenBit {
			mid := (lngRange[0] + lngRange[1]) / 2
			if lng >= mid {
				ch = ch<<1 | 1
				lngRange[0] = mid
			} else {
				ch = ch << 1
				lngRange[1] = mid
			}
		} else {
			mid := (latRange[0] + latRange[1]) / 2
			if lat >= mid {
				ch = ch<<1 | 1
				latRange[0] = mid
			} else {
				ch = ch << 1
				latRange[1] = mid
			}
		}
		evenBit = !evenBit

		if bit++; bit == 5 {
			out = append(out, base32[ch])
			bit, ch = 0, 0
		}
	}
	return string(out)
}

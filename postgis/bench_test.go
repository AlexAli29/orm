package postgis_test

import (
	"testing"

	"github.com/AlexAli29/orm/postgis"
)

// What the spatial path costs.
//
// The claim under test is that reading a geometry is a decode and not a
// reflection walk: one allocation for the coordinates, one for the parts when a
// shape has them, and nothing per ordinate. A codec that allocated per position
// would be invisible on a point and ruinous on a boundary with ten thousand
// vertices, which is exactly the shape a spatial application reads most.

func benchGeometry(vertices int) postgis.Geometry {
	ring := make([]postgis.Coord, 0, vertices+1)
	for i := range vertices {
		f := float64(i)
		ring = append(ring, postgis.Coord{X: f, Y: f * 2})
	}
	ring = append(ring, ring[0])
	return postgis.NewPolygon(4326, postgis.XY, ring)
}

func BenchmarkEWKB_encode(b *testing.B) {
	for _, n := range []int{4, 1000} {
		g := benchGeometry(n)
		buf := make([]byte, 0, len(g.EWKB()))
		b.Run(name(n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				buf = g.AppendEWKB(buf[:0])
			}
			_ = buf
		})
	}
}

func BenchmarkEWKB_decode(b *testing.B) {
	for _, n := range []int{4, 1000} {
		raw := benchGeometry(n).EWKB()
		b.Run(name(n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := postgis.DecodeEWKB(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func name(n int) string {
	if n == 4 {
		return "4 vertices"
	}
	return "1000 vertices"
}

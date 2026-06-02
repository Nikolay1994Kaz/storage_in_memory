//go:build amd64

package vector

import "golang.org/x/sys/cpu"

//go:noescape
func euclideanAVX2(a, b []float32) float32

//go:noescape
func dotProductAVX2(a, b []float32) float32

func init() {
	if cpu.X86.HasAVX2 {
		euclideanImpl = euclideanAVX2
		dotProductImpl = dotProductAVX2
	}
}

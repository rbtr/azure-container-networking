package ipampool

import "math"

func calculateTargetIPCount(demand, batch int64, minfree float64) int64 {
	return batch * int64(math.Ceil(minfree+float64(demand)/float64(batch)))
}

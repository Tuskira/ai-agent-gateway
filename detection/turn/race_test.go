//go:build race

package turn

func init() { slowdown = 25 } // the race detector slows the scan 10-20x, more under load

//go:build windows

package main

import (
	"sync"
	"testing"
)

// "keep awake" is asked from several places at once (each download manager's
// loop): under the race detector this used to report a data race.
func TestKeepAwakeFromManyPlacesAtOnce(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				setAwake((i+j)%2 == 0)
			}
		}(i)
	}
	wg.Wait()
	setAwake(false) // as it was
}

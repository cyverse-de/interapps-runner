package main

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
)

func portOkay(port int) bool {
	p, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	defer p.Close()
	if err != nil {
		return false
	}
	return true
}

// AvailableTCPPort will return an available port in the range specified by the
// lower and upper parameters. Returns an error if no available port exists.
func AvailableTCPPort(lower, upper int) (int, error) {
	attemptsMax := (upper - lower) + 1
	attemptsMade := 0
	portsTried := map[int]bool{}

	for attemptsMade < attemptsMax {
		rando := rand.Int()%(upper+1-lower) + lower
		if !portsTried[rando] { // Don't hammer the same port multiple times.
			if portOkay(rando) {
				return rando, nil
			}
			attemptsMade = attemptsMade + 1
			portsTried[rando] = true
		}
	}
	return -1, errors.New("no available port found")
}

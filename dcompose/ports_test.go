package dcompose

import (
	"fmt"
	"net"
	"testing"
)

func TestAvailableTCPPort(t *testing.T) {
	p, err := AvailableTCPPort(31300, 31399)
	if err != nil {
		t.Error(err)
	}

	if p > 31399 {
		t.Errorf("port was %d, which is greater than 31399", p)
	}

	if p < 31300 {
		t.Errorf("port was %d, which is less than 31300", p)
	}

	a, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
	defer a.Close()
	if err != nil {
		t.Error(err)
	}
}

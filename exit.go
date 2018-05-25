package main

import (
	"context"

	"gopkg.in/cyverse-de/messaging.v4"
)

// Exit handles clean up when road-runner is killed.
func Exit(cancel context.CancelFunc, exit, finalExit chan messaging.StatusCode) {
	exitCode := <-exit
	log.Warnf("Received an exit code of %d, cleaning up", int(exitCode))
	if exitCode != 0 {
		cancel()
	}
	finalExit <- exitCode
}

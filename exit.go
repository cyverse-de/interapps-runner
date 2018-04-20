package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/cyverse-de/messaging.v4"
)

func cleanup(cfg *viper.Viper) {
	var err error
	projName := strings.Replace(job.InvocationID, "-", "", -1) // dumb hack
	downCommand := exec.Command(cfg.GetString("docker-compose.path"), "-p", projName, "-f", "docker-compose.yml", "down", "-v")
	downCommand.Stderr = log.Writer()
	downCommand.Stdout = log.Writer()
	if err = downCommand.Run(); err != nil {
		log.Errorf("%+v\n", err)
	}

	baseURL := cfg.GetString("k8s.app-exposer.base")
	header := cfg.GetString("k8s.app-exposer.host-header")
	if err = DeleteK8SEndpoint(baseURL, header, job.InvocationID); err != nil {
		log.Errorf("%+v\n", err)
	}

	if err = DeleteK8SService(baseURL, header, job.InvocationID); err != nil {
		log.Errorf("%+v\n", err)
	}

	if err = DeleteK8SIngress(baseURL, header, job.InvocationID); err != nil {
		log.Errorf("%+v\n", err)
	}

	netCmd := exec.Command(cfg.GetString("docker.path"), "network", "rm", fmt.Sprintf("%s_default", projName))
	netCmd.Stderr = log.Writer()
	netCmd.Stdout = log.Writer()
	if err = netCmd.Run(); err != nil {
		log.Errorf("%+v\n", err)
	}
}

// Exit handles clean up when road-runner is killed.
func Exit(cfg *viper.Viper, exit, finalExit chan messaging.StatusCode) {
	exitCode := <-exit
	log.Warnf("Received an exit code of %d, cleaning up", int(exitCode))
	cleanup(cfg)
	finalExit <- exitCode
}

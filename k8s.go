package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
)

// EndpointConfig contains the values needed to set up an Endpoints object in
// the kubernetes cluster
type EndpointConfig struct {
	Port int    `json:"port"`
	IP   string `json:"ip"`
	Name string `json:",omit"`
}

// ServiceConfig contains the information needed to create a Service object in
// the Kubernetes cluster.
type ServiceConfig struct {
	TargetPort int    `json:"target_port"`
	ListenPort int    `json:"listen_port"`
	Name       string `json:",omit"`
}

// IngressConfig contains the information necessary to create an Ingress object
// in the Kubernetes cluster.
type IngressConfig struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
	Name    string `json:",omit"`
}

func postToAPI(u, host string, cfg interface{}) error {
	var (
		b   []byte
		err error
	)

	if b, err = json.Marshal(cfg); err != nil {
		return err
	}

	body := bytes.NewReader(b)
	req, err := http.NewRequest(http.MethodPost, u, body)
	if err != nil {
		return err
	}

	// Make sure the Host header is set so the Ingress knows how to route the
	// request.
	req.Header.Set(http.CanonicalHeaderKey("host"), host)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	if !(resp.StatusCode >= 200 && resp.StatusCode <= 299) {
		return fmt.Errorf("status code in response was %d", resp.StatusCode)
	}

	return nil
}

// CreateK8SEndpoint posts to the app-exposer API, which should create an
// Endpoint in the Kubernetes cluster.
func CreateK8SEndpoint(apiurl, host string, eptcfg *EndpointConfig) error {
	epturl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	epturl.Path = path.Join(epturl.Path, "endpoint", eptcfg.Name)
	return postToAPI(epturl.String(), host, eptcfg)
}

// CreateK8SService posts to the app-exposer API, which should create a
// Service in the Kubernetes cluster.
func CreateK8SService(apiurl, host string, svccfg *ServiceConfig) error {
	svcurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	svcurl.Path = path.Join(svcurl.Path, "service", svccfg.Name)
	return postToAPI(svcurl.String(), host, svccfg)
}

// CreateK8SIngress posts to the app-exposer API, which should create an
// Ingress in the Kubernetes cluster.
func CreateK8SIngress(apiurl, host string, ingcfg *IngressConfig) error {
	ingurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	ingurl.Path = path.Join(ingurl.Path, "ingress", ingcfg.Name)
	return postToAPI(ingurl.String(), host, ingcfg)
}

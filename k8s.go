package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
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
	req.Host = host

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	if !(resp.StatusCode >= 200 && resp.StatusCode <= 299) {
		respbody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %s", err.Error())
		}
		defer resp.Body.Close()
		return fmt.Errorf("status code in response was %d, body was: %s", resp.StatusCode, string(respbody))
	}

	return nil
}

func deleteToAPI(u, host string) error {
	var err error

	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}

	req.Host = host

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	if !(resp.StatusCode >= 200 && resp.StatusCode <= 299) {
		respbody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %s", err.Error())
		}
		defer resp.Body.Close()
		return fmt.Errorf("status code in response was %d, body was: %s", resp.StatusCode, string(respbody))
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

// DeleteK8SEndpoint deletes a K8S endpoint through the app-exposer API.
func DeleteK8SEndpoint(apiurl, host, name string) error {
	epturl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	epturl.Path = path.Join(epturl.Path, "endpoint", name)
	return deleteToAPI(epturl.String(), host)
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

// DeleteK8SService deletes a K8S service through the app-exposer API.
func DeleteK8SService(apiurl, host, name string) error {
	svcurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	svcurl.Path = path.Join(svcurl.Path, "service", name)
	return deleteToAPI(svcurl.String(), host)
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

// DeleteK8SIngress deletes a K8S ingress through the app-exposer API.
func DeleteK8SIngress(apiurl, host, name string) error {
	ingurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	ingurl.Path = path.Join(ingurl.Path, "ingress", name)
	return deleteToAPI(ingurl.String(), host)
}

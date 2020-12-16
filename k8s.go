package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
)

// HTTPDo performs an HTTP request in a new goroutine with the provided context.
// The request is performed synchronously. Adapted from: https://blog.golang.org/context.
func HTTPDo(ctx context.Context, req *http.Request, f func(*http.Response, error) error) error {
	c := make(chan error, 1)
	req = req.WithContext(ctx)

	go func() {
		c <- f(http.DefaultClient.Do(req))
	}()

	select {
	case <-ctx.Done():
		<-c
		return ctx.Err()
	case err := <-c:
		return err
	}
}

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

func postToAPI(ctx context.Context, u, host string, cfg interface{}) error {
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
	req.Header.Set("Content-Type", "application/json")

	// Make sure the Host header is set so the Ingress knows how to route the
	// request.
	req.Host = host

	err = HTTPDo(ctx, req, func(resp *http.Response, err error) error {
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if !(resp.StatusCode >= 200 && resp.StatusCode <= 299) {
			respbody, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %s", err.Error())
			}
			return fmt.Errorf("status code in response was %d, body was: %s", resp.StatusCode, string(respbody))
		}
		return nil
	})

	return err
}

func deleteToAPI(ctx context.Context, u, host string) error {
	var err error

	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}

	req.Host = host

	err = HTTPDo(ctx, req, func(resp *http.Response, err error) error {
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if !(resp.StatusCode >= 200 && resp.StatusCode <= 299) {
			respbody, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %s", err.Error())
			}
			defer resp.Body.Close()
			return fmt.Errorf("status code in response was %d, body was: %s", resp.StatusCode, string(respbody))
		}

		return nil
	})

	return err
}

// CreateK8SEndpoint posts to the app-exposer API, which should create an
// Endpoint in the Kubernetes cluster.
func CreateK8SEndpoint(ctx context.Context, apiurl, host string, eptcfg *EndpointConfig) error {
	epturl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	epturl.Path = path.Join(epturl.Path, "endpoint", eptcfg.Name)
	return postToAPI(ctx, epturl.String(), host, eptcfg)
}

// DeleteK8SEndpoint deletes a K8S endpoint through the app-exposer API.
func DeleteK8SEndpoint(ctx context.Context, apiurl, host, name string) error {
	epturl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	epturl.Path = path.Join(epturl.Path, "endpoint", name)
	return deleteToAPI(ctx, epturl.String(), host)
}

// CreateK8SService posts to the app-exposer API, which should create a
// Service in the Kubernetes cluster.
func CreateK8SService(ctx context.Context, apiurl, host string, svccfg *ServiceConfig) error {
	svcurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	svcurl.Path = path.Join(svcurl.Path, "service", svccfg.Name)
	return postToAPI(ctx, svcurl.String(), host, svccfg)
}

// DeleteK8SService deletes a K8S service through the app-exposer API.
func DeleteK8SService(ctx context.Context, apiurl, host, name string) error {
	svcurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	svcurl.Path = path.Join(svcurl.Path, "service", name)
	return deleteToAPI(ctx, svcurl.String(), host)
}

// CreateK8SIngress posts to the app-exposer API, which should create an
// Ingress in the Kubernetes cluster.
func CreateK8SIngress(ctx context.Context, apiurl, host string, ingcfg *IngressConfig) error {
	ingurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	ingurl.Path = path.Join(ingurl.Path, "ingress", ingcfg.Name)
	return postToAPI(ctx, ingurl.String(), host, ingcfg)
}

// DeleteK8SIngress deletes a K8S ingress through the app-exposer API.
func DeleteK8SIngress(ctx context.Context, apiurl, host, name string) error {
	ingurl, err := url.Parse(apiurl)
	if err != nil {
		return err
	}
	ingurl.Path = path.Join(ingurl.Path, "ingress", name)
	return deleteToAPI(ctx, ingurl.String(), host)
}

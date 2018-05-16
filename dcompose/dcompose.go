package dcompose

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"gopkg.in/cyverse-de/model.v2"
)

// WORKDIR is the path to the working directory inside all of the containers
// that are run as part of a job.
const WORKDIR = "/de-app-work"

// CONFIGDIR is the path to the local configs inside the containers that are
// used to transfer files into and out of the job.
const CONFIGDIR = "/configs"

// VOLUMEDIR is the name of the directory that is used for the working directory
// volume.
const VOLUMEDIR = "workingvolume"

// TMPDIR is the name of the directory that will be mounted into the container
// as the /tmp directory.
const TMPDIR = "tmpfiles"

const (
	UploadExcludesFilename string = "porklock-upload-exclusions.txt"

	// TypeLabel is the label key applied to every container.
	TypeLabel = "org.iplantc.containertype"

	// InputContainer is the value used in the TypeLabel for input containers.
	InputContainer = iota

	// DataContainer is the value used in the TypeLabel for data containers.
	DataContainer

	// StepContainer is the value used in the TypeLabel for step containers.
	StepContainer

	// OutputContainer is the value used in the TypeLabel for output containers.
	OutputContainer
)

var (
	logdriver      string
	hostworkingdir string
)

// IngressID returns the name/identifier for the ingress in k8s.
func IngressID(invocationID string) string {
	return fmt.Sprintf("app-%s", invocationID)
}

// FrontendURL generates the full URL to to the running app, as shown to the
// user and accessed through a browser.
func FrontendURL(invocationID, ingressID string, step *model.Step, cfg *viper.Viper) (string, error) {
	var (
		unmodifiedURL string
		fURL          *url.URL
		err           error
	)

	if step.Component.Container.InteractiveApps.FrontendURL != "" {
		unmodifiedURL = step.Component.Container.InteractiveApps.FrontendURL
	} else {
		unmodifiedURL = cfg.GetString("k8s.frontend.base")
	}

	fURL, err = url.Parse(unmodifiedURL)
	if err != nil {
		return "", errors.Wrapf(err, "error parsing URL %s", unmodifiedURL)
	}

	fURLPort := fURL.Port()
	fURLHost := fmt.Sprintf("%s.%s", ingressID, fURL.Hostname())

	if fURLPort != "" {
		fURL.Host = fmt.Sprintf("%s:%s", fURLHost, fURLPort)
	} else {
		fURL.Host = fURLHost
	}

	return fURL.String(), nil
}

// Volume is a Docker volume definition in the Docker compose file.
type Volume struct {
	Driver  string
	Options map[string]string `yaml:"driver_opts"`
}

// Network is a Docker network definition in the docker-compose file.
type Network struct {
	Driver string
	// EnableIPv6 bool              `yaml:"enable_ipv6"`
	DriverOpts map[string]string `yaml:"driver_opts"`
}

// LoggingConfig configures the logging for a docker-compose service.
type LoggingConfig struct {
	Driver  string
	Options map[string]string `yaml:"options,omitempty"`
}

// ServiceNetworkConfig configures a docker-compose service to use a Docker
// Network.
type ServiceNetworkConfig struct {
	Aliases []string `yaml:",omitempty"`
}

// Service configures a docker-compose service.
type Service struct {
	CapAdd        []string          `yaml:"cap_add,flow"`
	CapDrop       []string          `yaml:"cap_drop,flow"`
	Command       []string          `yaml:",omitempty"`
	ContainerName string            `yaml:"container_name,omitempty"`
	CPUSet        string            `yaml:"cpuset,omitempty"`
	CPUShares     int64             `yaml:"cpu_shares,omitempty"`
	CPUQuota      string            `yaml:"cpu_quota,omitempty"`
	DependsOn     []string          `yaml:"depends_on,omitempty"`
	Devices       []string          `yaml:",omitempty"`
	DNS           []string          `yaml:",omitempty"`
	DNSSearch     []string          `yaml:"dns_search,omitempty"`
	TMPFS         []string          `yaml:",omitempty"`
	EntryPoint    string            `yaml:",omitempty"`
	Environment   map[string]string `yaml:",omitempty"`
	Expose        []string          `yaml:",omitempty"`
	Image         string
	Labels        map[string]string                `yaml:",omitempty"`
	Logging       *LoggingConfig                   `yaml:",omitempty"`
	MemLimit      string                           `yaml:"mem_limit,omitempty"`
	MemSwapLimit  string                           `yaml:"memswap_limit,omitempty"`
	MemSwappiness string                           `yaml:"mem_swappiness,omitempty"`
	NetworkMode   string                           `yaml:"network_mode,omitempty"`
	Networks      map[string]*ServiceNetworkConfig `yaml:",omitempty"`
	PIDsLimit     int64                            `yaml:"pids_limit,omitempty"`
	Ports         []string                         `yaml:",omitempty"`
	Volumes       []string                         `yaml:",omitempty"`
	VolumesFrom   []string                         `yaml:"volumes_from,omitempty"`
	WorkingDir    string                           `yaml:"working_dir,omitempty"`
}

// JobCompose is the top-level type for what will become a job's docker-compose
// file.
type JobCompose struct {
	Version  string `yaml:"version"`
	Volumes  map[string]*Volume
	Networks map[string]*Network `yaml:",omitempty"`
	Services map[string]*Service
}

// New returns a newly instantiated *JobCompose instance.
func New(ld string, pathprefix string) (*JobCompose, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get host working directory")
	}

	logdriver = ld
	hostworkingdir = strings.TrimPrefix(wd, pathprefix)
	if strings.HasPrefix(hostworkingdir, "/") {
		hostworkingdir = strings.TrimPrefix(hostworkingdir, "/")
	}

	return &JobCompose{
		Version:  "2.1",
		Volumes:  make(map[string]*Volume),
		Networks: make(map[string]*Network),
		Services: make(map[string]*Service),
	}, nil
}

// InitFromJob fills out values as appropriate for running in the DE's Condor
// Cluster.
func (j *JobCompose) InitFromJob(job *model.Job, cfg *viper.Viper, workingdir string, availablePort int) error {
	var err error

	workingVolumeHostPath := path.Join(workingdir, VOLUMEDIR)
	// The volume containing the local working directory

	porklockImage := cfg.GetString("porklock.image")
	porklockTag := cfg.GetString("porklock.tag")
	porklockImageName := fmt.Sprintf("%s:%s", porklockImage, porklockTag)

	for index, dc := range job.DataContainers() {
		svcKey := fmt.Sprintf("data_%d", index)
		j.Services[svcKey] = &Service{
			Image:         fmt.Sprintf("%s:%s", dc.Name, dc.Tag),
			ContainerName: fmt.Sprintf("%s-%s", dc.NamePrefix, job.InvocationID),
			EntryPoint:    "/bin/true",
			Logging:       &LoggingConfig{Driver: "none"},
			Labels: map[string]string{
				model.DockerLabelKey: strconv.Itoa(DataContainer),
			},
		}

		svc := j.Services[svcKey]
		if dc.HostPath != "" || dc.ContainerPath != "" {
			var rw string
			if dc.ReadOnly {
				rw = "ro"
			} else {
				rw = "rw"
			}
			svc.Volumes = []string{
				fmt.Sprintf("%s:%s:%s", dc.HostPath, dc.ContainerPath, rw),
			}
		}
	}

	for index, input := range job.Inputs() {
		j.Services[fmt.Sprintf("input_%d", index)] = &Service{
			CapAdd:  []string{"IPC_LOCK"},
			Image:   porklockImageName,
			Command: input.Arguments(job.Submitter, job.FileMetadata),
			Environment: map[string]string{
				"VAULT_ADDR":  "${VAULT_ADDR}",
				"VAULT_TOKEN": "${VAULT_TOKEN}",
				"JOB_UUID":    job.InvocationID,
			},
			WorkingDir: WORKDIR,
			Volumes: []string{
				strings.Join([]string{workingVolumeHostPath, WORKDIR, "rw"}, ":"),
			},
			Labels: map[string]string{
				model.DockerLabelKey: strconv.Itoa(InputContainer),
			},
		}
	}

	// Add the steps to the docker-compose file.
	for index, step := range job.Steps {
		stepcfg := &ConvertStepParams{
			Step:               &step,
			Cfg:                cfg,
			Index:              index,
			User:               job.Submitter,
			InvID:              job.InvocationID,
			WorkingDirHostPath: workingVolumeHostPath,
			AvailablePort:      availablePort,
		}
		if err = j.ConvertStep(stepcfg); err != nil {
			return err
		}
	}

	// Add the final output job
	excludesPath := path.Join(workingdir, UploadExcludesFilename)
	excludesMount := path.Join(CONFIGDIR, UploadExcludesFilename)

	j.Services["upload_outputs"] = &Service{
		CapAdd:  []string{"IPC_LOCK"},
		Image:   porklockImageName,
		Command: job.FinalOutputArguments(excludesMount),
		Environment: map[string]string{
			"VAULT_ADDR":  "${VAULT_ADDR}",
			"VAULT_TOKEN": "${VAULT_TOKEN}",
			"JOB_UUID":    job.InvocationID,
		},
		WorkingDir: WORKDIR,
		Volumes: []string{
			strings.Join([]string{workingVolumeHostPath, WORKDIR, "rw"}, ":"),
			strings.Join([]string{excludesPath, excludesMount, "ro"}, ":"),
		},
		Labels: map[string]string{
			model.DockerLabelKey: job.InvocationID,
			TypeLabel:            strconv.Itoa(OutputContainer),
		},
	}
	return nil
}

func websocketURL(step *model.Step, backendURL string) (string, error) {
	var websocketURL string
	// if step.Component.Container.InteractiveApps.WebsocketPath != "" ||
	// 	step.Component.Container.InteractiveApps.WebsocketProto != "" ||
	// 	step.Component.Container.InteractiveApps.WebsocketPort != "" {

	burl, err := url.Parse(backendURL)
	if err != nil {
		return "", errors.Wrapf(err, "couldn't parse URL %s", backendURL)
	}

	var wsPath, wsProto, wsPort string
	if step.Component.Container.InteractiveApps.WebsocketPath != "" {
		wsPath = step.Component.Container.InteractiveApps.WebsocketPath
	} else {
		wsPath = burl.Path
	}

	if step.Component.Container.InteractiveApps.WebsocketProto != "" {
		wsProto = step.Component.Container.InteractiveApps.WebsocketProto
	} else {
		wsProto = burl.Scheme
	}

	if step.Component.Container.InteractiveApps.WebsocketPort != "" {
		wsPort = step.Component.Container.InteractiveApps.WebsocketPort
	} else {
		wsPort = burl.Port()
	}

	burl.Path = wsPath
	burl.Scheme = wsProto
	if wsPort != "" {
		burl.Host = fmt.Sprintf("%s:%s", burl.Hostname(), wsPort)
	}
	websocketURL = burl.String()
	// }
	return websocketURL, nil
}

// ConvertStepParams contains the info needed to call ConvertStep()
type ConvertStepParams struct {
	Step               *model.Step
	Cfg                *viper.Viper
	Index              int
	User               string
	InvID              string
	WorkingDirHostPath string
	AvailablePort      int
}

// ConvertStep will add the job step to the JobCompose services along with a
// proxy service.
func (j *JobCompose) ConvertStep(c *ConvertStepParams) error {
	var (
		step               = c.Step
		cfg                = c.Cfg
		index              = c.Index
		user               = c.User
		invID              = c.InvID
		workingDirHostPath = c.WorkingDirHostPath
		availablePort      = c.AvailablePort
	)

	// Construct the name of the image
	// Set the name of the image for the container.
	var imageName string
	if step.Component.Container.Image.Tag != "" {
		imageName = fmt.Sprintf(
			"%s:%s",
			step.Component.Container.Image.Name,
			step.Component.Container.Image.Tag,
		)
	} else {
		imageName = step.Component.Container.Image.Name
	}

	redirectURL, err := FrontendURL(invID, IngressID(invID), step, cfg)
	if err != nil {
		return err
	}

	step.Environment["IPLANT_USER"] = user
	step.Environment["IPLANT_EXECUTION_ID"] = invID
	step.Environment["REDIRECT_URL"] = redirectURL

	var containername string
	if step.Component.Container.Name != "" {
		containername = step.Component.Container.Name
	} else {
		containername = fmt.Sprintf("step_%d_%s", index, invID)
	}

	indexstr := strconv.Itoa(index)
	j.Services[fmt.Sprintf("step_%d", index)] = &Service{
		Image:      imageName,
		Command:    step.Arguments(),
		WorkingDir: step.Component.Container.WorkingDirectory(),
		Labels: map[string]string{
			model.DockerLabelKey: strconv.Itoa(StepContainer),
		},
		Logging: &LoggingConfig{
			Driver: logdriver,
			Options: map[string]string{
				"stderr": path.Join(hostworkingdir, VOLUMEDIR, step.Stderr(indexstr)),
				"stdout": path.Join(hostworkingdir, VOLUMEDIR, step.Stdout(indexstr)),
			},
		},
		ContainerName: containername,
		Environment:   step.Environment,
		VolumesFrom:   []string{},
		Volumes:       []string{},
		Devices:       []string{},
	}

	svc := j.Services[fmt.Sprintf("step_%d", index)]
	stepContainer := step.Component.Container

	for _, cp := range step.Component.Container.Ports {
		portstr := fmt.Sprintf("%d", cp.ContainerPort)
		svc.Ports = append(svc.Ports, portstr)
		svc.Expose = append(svc.Expose, portstr)
	}

	if stepContainer.EntryPoint != "" {
		svc.EntryPoint = stepContainer.EntryPoint
	}

	if stepContainer.MemoryLimit > 0 {
		svc.MemLimit = strconv.FormatInt(stepContainer.MemoryLimit, 10)
	}

	if stepContainer.CPUShares > 0 {
		svc.CPUShares = stepContainer.CPUShares
	}

	if stepContainer.PIDsLimit > 0 {
		svc.PIDsLimit = stepContainer.PIDsLimit
	}

	// Handles volumes created by other containers.
	for _, vf := range stepContainer.VolumesFrom {
		containerName := fmt.Sprintf("%s-%s", vf.NamePrefix, invID)
		var foundService string
		for svckey, svc := range j.Services { // svckey is the docker-compose service name.
			if svc.ContainerName == containerName {
				foundService = svckey
			}
		}
		svc.VolumesFrom = append(svc.VolumesFrom, foundService)
	}

	// The working directory needs to be mounted as a volume.
	svc.Volumes = append(svc.Volumes, strings.Join([]string{workingDirHostPath, stepContainer.WorkingDirectory(), "rw"}, ":"))

	// The TMPDIR needs to be mounted as a volume
	if !step.Component.Container.SkipTmpMount {
		svc.Volumes = append(svc.Volumes, fmt.Sprintf("./%s:/tmp:rw", TMPDIR))
	}

	for _, v := range stepContainer.Volumes {
		var rw string
		if v.ReadOnly {
			rw = "ro"
		} else {
			rw = "rw"
		}
		if v.HostPath == "" {
			svc.Volumes = append(svc.Volumes, fmt.Sprintf("%s:%s", v.ContainerPath, rw))
		} else {
			svc.Volumes = append(svc.Volumes, fmt.Sprintf("%s:%s:%s", v.HostPath, v.ContainerPath, rw))
		}
	}

	for _, device := range stepContainer.Devices {
		svc.Devices = append(svc.Devices,
			fmt.Sprintf("%s:%s:%s",
				device.HostPath,
				device.ContainerPath,
				device.CgroupPermissions,
			),
		)
	}

	/////////// Proxy

	//Only supporting a single port for now.
	var containerPort int
	if step.Component.Container.Ports[0].ContainerPort != 0 {
		containerPort = step.Component.Container.Ports[0].ContainerPort
	}

	var backendURL string
	if step.Component.Container.InteractiveApps.BackendURL != "" {
		backendURL = step.Component.Container.InteractiveApps.BackendURL
	} else {
		if containerPort != 0 {
			backendURL = fmt.Sprintf("http://step_%d_%s:%d", index, invID, containerPort)
		} else {
			backendURL = fmt.Sprintf("http://step_%d_%s", index, invID)
		}
	}

	websocketURL, err := websocketURL(step, backendURL)
	if err != nil {
		return err
	}

	ingressID := IngressID(invID)

	frontendURL, err := FrontendURL(invID, ingressID, step, cfg)
	if err != nil {
		return err
	}

	// Add a service for the proxy container. Each step has a corresponding proxy.
	proxyName := ProxyName(index, invID)
	j.Services[ProxyServiceName(index)] = &Service{
		Image: stepContainer.InteractiveApps.ProxyImage,
		Command: []string{
			"--backend-url", backendURL,
			"--ws-backend-url", websocketURL,
			"--frontend-url", frontendURL,
			"--cas-base-url", stepContainer.InteractiveApps.CASURL,
			"--cas-validate", stepContainer.InteractiveApps.CASValidate,
		},
		Labels: map[string]string{
			model.DockerLabelKey: strconv.Itoa(StepContainer),
		},
		Logging: &LoggingConfig{
			Driver: logdriver,
			Options: map[string]string{
				"stderr": path.Join(hostworkingdir, VOLUMEDIR, step.Stderr(indexstr)),
				"stdout": path.Join(hostworkingdir, VOLUMEDIR, step.Stdout(indexstr)),
			},
		},
		ContainerName: proxyName,
		Environment:   step.Environment,
		Ports: []string{
			fmt.Sprintf("%s:8080", strconv.Itoa(availablePort)),
		},
	}
	return nil
}

// ProxyName returns the name of the cas-proxy container based on the step index and the
// job's invocationID.
func ProxyName(index int, invocationID string) string {
	return fmt.Sprintf("step_proxy_%d_%s", index, invocationID)
}

// ProxyServiceName returns the docker-compose service name for the cas-proxy,
// based on the step index passed in.
func ProxyServiceName(index int) string {
	return fmt.Sprintf("proxy_%d", index)
}

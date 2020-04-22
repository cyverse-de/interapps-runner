package main

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"gopkg.in/cyverse-de/model.v5"
)

// WORKDIR is the path to the working directory inside all of the containers
// that are run as part of a job.
const WORKDIR = "/de-app-work"

// IRODSCONFIGNAME is the basename of the irods config file
const IRODSCONFIGNAME = "irods-config"

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
	// UploadExcludesFilename is the filename for the list of files that should
	// not be uploaded when the app is finished executing.
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

// JobComposition is the top-level type for what will become a job's docker-compose
// file.
type JobComposition struct {
	Version  string `yaml:"version"`
	Volumes  map[string]*Volume
	Networks map[string]*Network `yaml:",omitempty"`
	Services map[string]*Service
}

// NewJobComposition returns a newly instantiated *JobComposition instance.
func NewJobComposition(ld string, pathprefix string) (*JobComposition, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get host working directory")
	}

	logdriver = ld
	hostworkingdir = strings.TrimPrefix(wd, pathprefix)
	if strings.HasPrefix(hostworkingdir, "/") {
		hostworkingdir = strings.TrimPrefix(hostworkingdir, "/")
	}

	return &JobComposition{
		Version:  "2.2",
		Volumes:  make(map[string]*Volume),
		Networks: make(map[string]*Network),
		Services: make(map[string]*Service),
	}, nil
}

// Composer orchestrates the creation of the docker-compose file for a job.
type Composer struct {
	job             *model.Job
	composition     *JobComposition
	ingressID       string // This shouldn't be accessed directly. Use IngressID().
	porklockImage   string
	porklockTag     string
	memoryLimit     int64
	maxCPUCores     float64
	frontendBaseURL string
	ingressURL      string
	analysisHeader  string
	accessHeader    string
}

// NewComposer returns a new *Composer.
func NewComposer(job *model.Job, cfg *viper.Viper, logDriver, pathPrefix string) (*Composer, error) {
	c, err := NewJobComposition(logDriver, pathPrefix)
	if err != nil {
		return nil, err
	}
	return &Composer{
		job:             job,
		composition:     c,
		porklockImage:   cfg.GetString(ConfigPorklockImageKey),
		porklockTag:     cfg.GetString(ConfigPorklockTagKey),
		memoryLimit:     cfg.GetInt64(ConfigMemoryLimitKey),
		maxCPUCores:     cfg.GetFloat64(ConfigMaxCPUCoresKey),
		frontendBaseURL: cfg.GetString(ConfigFrontendBaseKey),
		ingressURL:      cfg.GetString(ConfigAppExposerBaseKey),
		analysisHeader:  cfg.GetString(ConfigAnalysisHeaderKey),
		accessHeader:    cfg.GetString(ConfigAccessHeaderKey),
	}, nil
}

// IngressID returns the name/identifier for the ingress in k8s.
func (c *Composer) IngressID() string {
	if c.ingressID == "" {
		c.ingressID = fmt.Sprintf("a%x", sha256.Sum256([]byte(fmt.Sprintf("%s%s", c.job.UserID, c.job.InvocationID))))[0:9]
	}
	return c.ingressID
}

// FrontendURL generates the full URL to to the running app, as shown to the
// user and accessed through a browser.
func (c *Composer) FrontendURL(step *model.Step) (string, error) {
	var (
		unmodifiedURL string
		fURL          *url.URL
		err           error
	)

	if step.Component.Container.InteractiveApps.FrontendURL != "" {
		unmodifiedURL = step.Component.Container.InteractiveApps.FrontendURL
	} else {
		unmodifiedURL = c.frontendBaseURL
	}

	fURL, err = url.Parse(unmodifiedURL)
	if err != nil {
		return "", errors.Wrapf(err, "error parsing URL %s", unmodifiedURL)
	}

	fURLPort := fURL.Port()
	fURLHost := fmt.Sprintf("%s.%s", c.IngressID(), fURL.Hostname())

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
	CPUs          string            `yaml:"cpus,omitempty"`
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

// InitFromJob fills out values as appropriate for running in the DE's Condor
// Cluster.
func (c *Composer) InitFromJob(workingdir string, availablePort int) error {
	var err error

	workingVolumeHostPath := path.Join(workingdir, VOLUMEDIR)
	irodsConfigPath := path.Join(workingdir, IRODSCONFIGNAME)

	porklockImageName := fmt.Sprintf("%s:%s", c.porklockImage, c.porklockTag)

	for index, dc := range job.DataContainers() {
		svcKey := fmt.Sprintf("data_%d", index)
		c.composition.Services[svcKey] = &Service{
			Image:         fmt.Sprintf("%s:%s", dc.Name, dc.Tag),
			ContainerName: fmt.Sprintf("%s-%s", dc.NamePrefix, job.InvocationID),
			EntryPoint:    "/bin/true",
			Logging:       &LoggingConfig{Driver: "none"},
			Labels: map[string]string{
				model.DockerLabelKey: strconv.Itoa(DataContainer),
			},
		}

		svc := c.composition.Services[svcKey]
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
		c.composition.Services[fmt.Sprintf("input_%d", index)] = &Service{
			CapAdd:  []string{"IPC_LOCK"},
			Image:   porklockImageName,
			Command: input.Arguments(job.Submitter, job.FileMetadata),
			Environment: map[string]string{
				"JOB_UUID": job.InvocationID,
			},
			WorkingDir: WORKDIR,
			Volumes: []string{
				strings.Join([]string{workingVolumeHostPath, WORKDIR, "rw"}, ":"),
				strings.Join([]string{irodsConfigPath, path.Join(CONFIGDIR, IRODSCONFIGNAME), "ro"}, ":"),
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
			Index:              index,
			User:               job.Submitter,
			UserID:             job.UserID,
			InvID:              job.InvocationID,
			WorkingDirHostPath: workingVolumeHostPath,
			AvailablePort:      availablePort,
		}
		if err = c.ConvertStep(stepcfg); err != nil {
			return err
		}
	}

	// Add the final output job
	excludesPath := path.Join(workingdir, UploadExcludesFilename)
	excludesMount := path.Join(CONFIGDIR, UploadExcludesFilename)

	c.composition.Services["upload_outputs"] = &Service{
		CapAdd:  []string{"IPC_LOCK"},
		Image:   porklockImageName,
		Command: job.FinalOutputArguments(excludesMount),
		Environment: map[string]string{
			"JOB_UUID": job.InvocationID,
		},
		WorkingDir: WORKDIR,
		Volumes: []string{
			strings.Join([]string{workingVolumeHostPath, WORKDIR, "rw"}, ":"),
			strings.Join([]string{irodsConfigPath, path.Join(CONFIGDIR, IRODSCONFIGNAME), "ro"}, ":"),
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
	Index              int
	User               string
	UserID             string
	InvID              string
	WorkingDirHostPath string
	AvailablePort      int
}

// ConvertStep will add the job step to the JobComposition services along with a
// proxy service.
func (c *Composer) ConvertStep(s *ConvertStepParams) error {
	var (
		step               = s.Step
		index              = s.Index
		user               = s.User
		workingDirHostPath = s.WorkingDirHostPath
		availablePort      = s.AvailablePort
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

	redirectURL, err := c.FrontendURL(step)
	if err != nil {
		return err
	}

	step.Environment["IPLANT_USER"] = user
	step.Environment["IPLANT_EXECUTION_ID"] = c.job.InvocationID
	step.Environment["REDIRECT_URL"] = redirectURL

	var containername string
	if step.Component.Container.Name != "" {
		containername = step.Component.Container.Name
	} else {
		containername = fmt.Sprintf("step-%d-%s", index, c.job.InvocationID)
	}

	indexstr := strconv.Itoa(index)
	c.composition.Services[fmt.Sprintf("step_%d", index)] = &Service{
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

	svc := c.composition.Services[fmt.Sprintf("step_%d", index)]
	stepContainer := step.Component.Container

	for _, cp := range step.Component.Container.Ports {
		portstr := fmt.Sprintf("%d", cp.ContainerPort)
		svc.Ports = append(svc.Ports, portstr)
		svc.Expose = append(svc.Expose, portstr)
	}

	if stepContainer.EntryPoint != "" {
		svc.EntryPoint = stepContainer.EntryPoint
	}

	// If a memory limit is set, use it. Otherwise default to allocating 4GB of
	// RAM for the container. For now we won't worry about the swap limit.
	if stepContainer.MemoryLimit > 0 {
		svc.MemLimit = fmt.Sprintf("%db", stepContainer.MemoryLimit)
	} else {
		svc.MemLimit = fmt.Sprintf("%db", c.memoryLimit)
	}

	if stepContainer.CPUShares > 0 {
		svc.CPUShares = stepContainer.CPUShares
	}

	if stepContainer.PIDsLimit > 0 {
		svc.PIDsLimit = stepContainer.PIDsLimit
	}

	// If the MaxCPUCores setting is provided, use it. Otherwise, default to
	// limiting the container to 2.0 cores.
	if stepContainer.MaxCPUCores > 0.0 {
		svc.CPUs = fmt.Sprintf("%f", stepContainer.MaxCPUCores)
	} else {
		svc.CPUs = fmt.Sprintf("%f", c.maxCPUCores)
	}

	// Handles volumes created by other containers.
	for _, vf := range stepContainer.VolumesFrom {
		containerName := fmt.Sprintf("%s-%s", vf.NamePrefix, c.job.InvocationID)
		var foundService string
		for svckey, svc := range c.composition.Services { // svckey is the docker-compose service name.
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
			backendURL = fmt.Sprintf("http://step-%d-%s:%d", index, c.job.InvocationID, containerPort)
		} else {
			backendURL = fmt.Sprintf("http://step-%d-%s", index, c.job.InvocationID)
		}
	}

	websocketURL, err := websocketURL(step, backendURL)
	if err != nil {
		return err
	}

	frontendURL, err := c.FrontendURL(step)
	if err != nil {
		return err
	}

	// Add a service for the proxy container. Each step has a corresponding proxy.
	proxyName := c.ProxyName(index)
	c.composition.Services[ProxyServiceName(index)] = &Service{
		Image: stepContainer.InteractiveApps.ProxyImage,
		Command: []string{
			"--backend-url", backendURL,
			"--ws-backend-url", websocketURL,
			"--frontend-url", frontendURL,
			"--cas-base-url", stepContainer.InteractiveApps.CASURL,
			"--cas-validate", stepContainer.InteractiveApps.CASValidate,
			"--external-id", c.job.InvocationID,
			"--ingress-url", c.ingressURL,
			"--analysis-header", c.analysisHeader,
			"--access-header", c.accessHeader,
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
func (c *Composer) ProxyName(index int) string {
	return fmt.Sprintf("step_proxy_%d_%s", index, c.job.InvocationID)
}

// ProxyServiceName returns the docker-compose service name for the cas-proxy,
// based on the step index passed in.
func ProxyServiceName(index int) string {
	return fmt.Sprintf("proxy_%d", index)
}
